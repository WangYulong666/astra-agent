package remotedesk

// 管理器实现：配对/设备/会话/审批/TTL/运行时装配/能力探测。
//
// 分层状态（为什么这么设计）：
//
//	Device  ──1:N──▶  Session  ──1:N──▶  Runtime（每端一个订阅）
//
// Device 是长期关系（配对过一次就一直在，除非吊销），Session 是短期运行时状态
// （有 TTL，进程重启即作废），Runtime 是一次 WebSocket 连接（多端可同时看同一路）。
// 抓屏后端按 Manager 共享一份：每会话起一个 screencapture 进程会直接打爆机器。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ttlSweepInterval 是会话 TTL 扫描周期。10 秒足够精确（会话 TTL 是小时级），
	// 又不至于让每个 Manager 都高频唤醒。
	ttlSweepInterval = 10 * time.Second
	// capabilityTTL 是能力探测结果缓存时间（契约要求 30 秒）。
	capabilityTTL = 30 * time.Second
	// 列表分页：limit<=0 用 20，上限 100。
	defaultListLimit = 20
	maxListLimit     = 100
)

type manager struct {
	dir     string
	pol     Policy
	tenancy bool
	lg      *log.Logger
	audit   AuditSink

	captureFactory CaptureFactory
	inputFactory   InputFactory

	mu       sync.RWMutex
	devices  map[string]*Device
	sessions map[string]*Session
	closed   bool

	// 配对码是纯内存状态（一次性、120 秒），不落盘：重启后旧码本来就该失效。
	pairMu   sync.Mutex
	pairings map[string]*pairState

	// 共享后端。按 Manager 一份，首次用到时懒创建。
	bkMu    sync.Mutex
	capture Capture
	input   Input

	// 已打开的运行时，按会话聚合，Close/EndSession 时要能全部关掉。
	rtMu     sync.Mutex
	runtimes map[string][]*trackedRuntime

	capMu     sync.Mutex
	capCached Capabilities
	capAt     time.Time

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// trackedRuntime 给 Runtime 加一层「退订时自动从管理器注销」。
// 没有它的话，反复 Open/Close 会让 runtimes 切片无限增长（每个会话记住所有历史订阅）。
type trackedRuntime struct {
	Runtime
	m      *manager
	sessID string
	once   sync.Once
}

func (t *trackedRuntime) Close() error {
	err := t.Runtime.Close()
	t.once.Do(func() { t.m.dropRuntime(t.sessID, t) })
	return err
}

func openImpl(cfg Config) (Manager, error) {
	if cfg.Disabled {
		return nil, fmt.Errorf("remotedesk: 已被 ASTRA_REMOTE_DISABLED 关闭")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, fmt.Errorf("remotedesk: 缺少 DataDir，无法保存设备/会话")
	}
	dir := filepath.Join(cfg.DataDir, "remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("remotedesk: 创建数据目录 %s 失败: %w", dir, err)
	}

	lg := cfg.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	m := &manager{
		dir:            dir,
		pol:            cfg.Policy.WithDefaults(),
		tenancy:        cfg.Tenancy,
		lg:             lg,
		audit:          cfg.Audit,
		captureFactory: cfg.Capture,
		inputFactory:   cfg.Input,
		devices:        make(map[string]*Device),
		sessions:       make(map[string]*Session),
		pairings:       make(map[string]*pairState),
		runtimes:       make(map[string][]*trackedRuntime),
		stop:           make(chan struct{}),
	}
	if m.captureFactory == nil {
		m.captureFactory = defaultCapture
	}
	if m.inputFactory == nil {
		m.inputFactory = defaultInput
	}

	// 读坏了必须失败：静默当首启会让用户以为配对关系凭空消失。
	if err := m.loadStore(); err != nil {
		return nil, err
	}
	// 首启文件不存在，不必写；已有数据则把「重启改 ended」的变化落盘。
	if err := m.saveDevices(); err != nil {
		return nil, err
	}
	if err := m.saveSessions(); err != nil {
		return nil, err
	}

	m.wg.Add(1)
	go m.ttlLoop()
	return m, nil
}

// ── 通用工具 ────────────────────────────────────────────────────

func (m *manager) logf(format string, args ...any) {
	if m.lg != nil {
		m.lg.Printf(format, args...)
	}
}

func (m *manager) emitAudit(action, target string, detail map[string]any) {
	if m.audit == nil {
		return
	}
	// 审计回调由外部注入，可能慢/可能 panic，绝不能让它带着锁跑。
	defer func() { _ = recover() }()
	m.audit(action, target, detail)
}

func (m *manager) ensureOpen() error {
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return fmt.Errorf("%w: 远程桌面已关闭", ErrUnavailable)
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newDeviceID() string {
	h, err := randomHex(6) // 12 位 hex
	if err != nil {
		// crypto/rand 失败说明系统熵池有问题，这里退化成时间戳也比崩溃好；
		// 但设备 ID 不承担安全职责（配对码才是），所以可接受。
		return "dev_" + fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return "dev_" + h
}

func newSessionID() string {
	h, err := randomHex(8)
	if err != nil {
		return "sess_" + fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return "sess_" + h
}

func cloneDevice(d *Device) Device {
	c := *d
	c.RevokedAt = cloneTime(d.RevokedAt)
	return c
}

func cloneSession(s *Session) Session {
	c := *s
	c.ApprovedAt = cloneTime(s.ApprovedAt)
	c.EndedAt = cloneTime(s.EndedAt)
	return c
}

// ── 共享抓屏 / 注入后端 ─────────────────────────────────────────

// sharedCapture 懒创建整机唯一的一份抓屏后端。
func (m *manager) sharedCapture() (Capture, error) {
	m.bkMu.Lock()
	defer m.bkMu.Unlock()
	if m.capture != nil {
		return m.capture, nil
	}
	c, err := m.captureFactory(m.cfgForFactory())
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("抓屏后端返回空实现")
	}
	m.capture = c
	return c, nil
}

func (m *manager) sharedInput() (Input, error) {
	m.bkMu.Lock()
	defer m.bkMu.Unlock()
	if m.input != nil {
		return m.input, nil
	}
	in, err := m.inputFactory(m.cfgForFactory())
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("注入后端返回空实现")
	}
	m.input = in
	return in, nil
}

func (m *manager) cfgForFactory() Config {
	return Config{
		DataDir: m.dir,
		Tenancy: m.tenancy,
		Policy:  m.pol,
		Logger:  m.lg,
	}
}

func (m *manager) closeBackends() {
	m.bkMu.Lock()
	c, in := m.capture, m.input
	m.capture, m.input = nil, nil
	m.bkMu.Unlock()
	if c != nil {
		if err := c.Close(); err != nil {
			m.logf("关闭抓屏后端失败: %v", err)
		}
	}
	if in != nil {
		if err := in.Close(); err != nil {
			m.logf("关闭注入后端失败: %v", err)
		}
	}
}

// inputFor 决定本次会话拿到的注入后端。
//
// 注意「不能控」有两种原因，用户看到的提示必须不同：
//  1. 本会话只是看（view）——这是设计如此；
//  2. 本机没有辅助功能权限（control 但后端不可用）——这是要去系统设置里开的。
//
// 两种都返回非 nil 的占位实现，保证 Runtime 不必到处判空。
func (m *manager) inputFor(mode string) Input {
	if mode != ModeControl {
		return unavailableInput{cap: Capability{
			Available: false,
			Reason:    "当前会话为只看模式，未授权输入接管",
		}}
	}
	in, err := m.sharedInput()
	if err != nil {
		m.logf("注入后端不可用，本会话降级为只看: %v", err)
		return unavailableInput{cap: Capability{Available: false, Reason: err.Error()}}
	}
	if info := in.Info(); !info.Available {
		if info.Reason == "" {
			info.Reason = "本机未授予辅助功能权限"
		}
		return unavailableInput{cap: info}
	}
	return in
}

// ── 会话 ────────────────────────────────────────────────────────

func (m *manager) StartSession(ctx context.Context, req StartRequest) (Session, error) {
	if err := m.ensureOpen(); err != nil {
		return Session{}, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}

	mode := strings.TrimSpace(req.Mode)
	switch mode {
	case ModeView, ModeControl:
	default:
		return Session{}, fmt.Errorf("%w: 未知会话模式 %q", ErrDenied, req.Mode)
	}
	if mode == ModeControl && m.pol.DisableInput {
		// 明确告诉用户「本机没开输入接管」，而不是建个 control 会话然后在注入时才失败。
		return Session{}, fmt.Errorf("%w: 本机未开启输入接管", ErrUnavailable)
	}

	// 抓屏不可用时开出来的会话只能是黑屏，不如现在就如实拒绝（API 翻成 503 + 原因）。
	capBackend, err := m.sharedCapture()
	if err != nil {
		return Session{}, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	if info := capBackend.Info(); !info.Available {
		reason := info.Reason
		if reason == "" {
			reason = "抓屏后端不可用"
		}
		return Session{}, fmt.Errorf("%w: %s", ErrUnavailable, reason)
	}

	// 默认审批：control 看策略（默认 true），view 默认免批（只看画面风险低，走审批会让
	// 「人不在跟前想看一眼」这个主场景变卡）。
	needApproval := false
	if mode == ModeControl {
		needApproval = m.pol.ControlNeedsApproval
	}
	if req.RequireApproval != nil {
		needApproval = *req.RequireApproval
	}

	now := time.Now()
	sess := Session{
		ID:        newSessionID(),
		TenantID:  req.TenantID,
		UserID:    req.UserID,
		DeviceID:  req.DeviceID,
		Mode:      mode,
		Status:    StatusActive,
		CreatedAt: now,
		ExpiresAt: now.Add(m.pol.SessionTTL),
		Client:    req.Client,
	}

	var superseded []string
	var devID string
	m.mu.Lock()
	dev, ok := m.devices[req.DeviceID]
	if !ok || dev.TenantID != req.TenantID || dev.UserID != req.UserID || dev.RevokedAt != nil {
		m.mu.Unlock()
		// 跨租户/已吊销一律「不存在」，不给探测留口子。
		return Session{}, ErrNotFound
	}
	// 锁内取出，出锁后只用局部值：否则并发吊销会改 *Device，审计读到的是竞态数据。
	devID = dev.ID
	sess.Device = dev.Name
	if needApproval {
		sess.Status = StatusPending
	} else {
		sess.ApprovedAt = &now
	}
	// 同一设备 + 同一模式只保留最新一条：否则「多开几次」会堆一地僵尸会话，
	// 用户看到设备在线但其实连的是旧会话。
	for _, s := range m.sessions {
		if s.DeviceID == dev.ID && s.Mode == mode && !isFinished(s.Status) {
			s.Status = StatusEnded
			s.EndedAt = &now
			s.EndReason = ReasonSuperseded
			superseded = append(superseded, s.ID)
		}
	}
	dev.LastSeenAt = now
	m.sessions[sess.ID] = &sess
	m.pruneSessionsLocked()
	err = m.saveSessionsLocked()
	if err == nil {
		err = m.saveDevicesLocked()
	}
	out := cloneSession(&sess)
	m.mu.Unlock()
	if err != nil {
		return Session{}, err
	}

	m.closeRuntimes(superseded...)
	m.emitAudit("remote.session.start", sess.ID, map[string]any{
		"device": devID,
		"mode":   mode,
		"client": req.Client,
		"status": out.Status,
	})
	return out, nil
}

func (m *manager) GetSession(ctx context.Context, tenantID, sessionID string) (Session, error) {
	if err := m.ensureOpen(); err != nil {
		return Session{}, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	m.mu.RLock()
	s, ok := m.sessions[sessionID]
	if !ok || s.TenantID != tenantID {
		m.mu.RUnlock()
		return Session{}, ErrNotFound
	}
	out := cloneSession(s)
	m.mu.RUnlock()
	out.Stats = m.statsFor(sessionID, out.Stats)
	return out, nil
}

func (m *manager) ListSessions(ctx context.Context, tenantID string, limit int) ([]Session, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	m.mu.RLock()
	all := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.TenantID != tenantID {
			continue
		}
		all = append(all, cloneSession(s))
	}
	m.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	if len(all) > limit {
		all = all[:limit]
	}
	for i := range all {
		all[i].Stats = m.statsFor(all[i].ID, all[i].Stats)
	}
	return all, nil
}

func (m *manager) ApproveSession(ctx context.Context, tenantID, sessionID string) (Session, error) {
	if err := m.ensureOpen(); err != nil {
		return Session{}, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok || s.TenantID != tenantID {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	switch s.Status {
	case StatusActive:
		// 幂等：桌面端双点/重连重放不该报错。
		out := cloneSession(s)
		m.mu.Unlock()
		return out, nil
	case StatusPending:
	default:
		m.mu.Unlock()
		return Session{}, fmt.Errorf("%w: 会话状态 %s 不可批准", ErrDenied, s.Status)
	}
	now := time.Now()
	s.Status = StatusActive
	s.ApprovedAt = &now
	err := m.saveSessionsLocked()
	out := cloneSession(s)
	deviceID := s.DeviceID
	m.mu.Unlock()
	if err != nil {
		return Session{}, err
	}

	m.emitAudit("remote.control.grant", sessionID, map[string]any{
		"device": deviceID,
		"mode":   out.Mode,
	})
	return out, nil
}

func (m *manager) EndSession(ctx context.Context, tenantID, sessionID, reason string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = ReasonUserEnded
	}

	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok || s.TenantID != tenantID {
		m.mu.Unlock()
		return ErrNotFound
	}
	if isFinished(s.Status) {
		m.mu.Unlock()
		return nil // 幂等：任一方都能断，重复断不算错
	}
	now := time.Now()
	s.Status = StatusEnded
	s.EndedAt = &now
	s.EndReason = reason
	err := m.saveSessionsLocked()
	deviceID := s.DeviceID
	m.mu.Unlock()
	if err != nil {
		return err
	}

	m.closeRuntimes(sessionID)
	m.emitAudit("remote.session.end", sessionID, map[string]any{
		"device": deviceID,
		"reason": reason,
	})
	return nil
}

// sweepExpired 处理 TTL 到期。抽成方法而不是塞进循环，测试可以直接调。
func (m *manager) sweepExpired() {
	now := time.Now()
	var expired []string
	m.mu.Lock()
	changed := false
	for _, s := range m.sessions {
		if isFinished(s.Status) || !now.After(s.ExpiresAt) {
			continue
		}
		s.Status = StatusExpired
		s.EndedAt = &now
		s.EndReason = ReasonTTL
		expired = append(expired, s.ID)
		changed = true
	}
	if changed {
		if err := m.saveSessionsLocked(); err != nil {
			m.logf("会话 TTL 落盘失败: %v", err)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 {
		return
	}
	m.closeRuntimes(expired...)
	for _, id := range expired {
		m.emitAudit("remote.session.end", id, map[string]any{"reason": ReasonTTL})
	}
}

func (m *manager) ttlLoop() {
	defer m.wg.Done()
	t := time.NewTicker(ttlSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sweepExpired()
		}
	}
}

// ── 运行时 ──────────────────────────────────────────────────────

func (m *manager) Open(ctx context.Context, tenantID, sessionID string) (Runtime, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	s, ok := m.sessions[sessionID]
	var snap Session
	if ok {
		snap = cloneSession(s)
	}
	m.mu.RUnlock()
	if !ok || snap.TenantID != tenantID {
		return nil, ErrNotFound
	}
	switch snap.Status {
	case StatusActive:
	case StatusPending:
		return nil, fmt.Errorf("%w: 会话等待桌面端批准", ErrDenied)
	default:
		return nil, ErrNotFound
	}

	capBackend, err := m.sharedCapture()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	if info := capBackend.Info(); !info.Available {
		reason := info.Reason
		if reason == "" {
			reason = "抓屏后端不可用"
		}
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, reason)
	}
	in := m.inputFor(snap.Mode)

	rt, err := newRuntime(snap, m.pol, capBackend, in, m.lg, func(reason string) {
		m.fatalStream(sessionID, reason)
	})
	if err != nil {
		return nil, err
	}
	t := &trackedRuntime{Runtime: rt, m: m, sessID: sessionID}
	m.rtMu.Lock()
	m.runtimes[sessionID] = append(m.runtimes[sessionID], t)
	m.rtMu.Unlock()
	return t, nil
}

// fatalStream 由抓屏泵在不可恢复失败时回调（stream.go 的 onFatal）。
//
// 刻意**不在这里 Close 运行时**：回调是从泵自己的 goroutine 里出来的，重入
// Runtime.Close 很容易和泵内部的锁/等待互相咬死。只把会话标成 ended，
// 订阅方看到通道关闭自然会退订，最终由 EndSession/Close 统一收尾。
func (m *manager) fatalStream(sessionID, reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = ReasonStreamError
	}
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok || isFinished(s.Status) {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	s.Status = StatusEnded
	s.EndedAt = &now
	s.EndReason = reason
	deviceID := s.DeviceID
	err := m.saveSessionsLocked()
	m.mu.Unlock()

	if err != nil {
		m.logf("会话 %s 因抓屏失败结束，但落盘失败: %v", sessionID, err)
	}
	m.logf("会话 %s 抓屏泵不可恢复失败（%s），已结束", sessionID, reason)
	m.emitAudit("remote.session.end", sessionID, map[string]any{
		"device": deviceID,
		"reason": reason,
	})
}

func (m *manager) dropRuntime(sessionID string, t *trackedRuntime) {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	list := m.runtimes[sessionID]
	for i, rt := range list {
		if rt == t {
			m.runtimes[sessionID] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(m.runtimes[sessionID]) == 0 {
		delete(m.runtimes, sessionID)
	}
}

func (m *manager) closeRuntimes(sessionIDs ...string) {
	if len(sessionIDs) == 0 {
		return
	}
	want := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		want[id] = struct{}{}
	}
	var victims []*trackedRuntime
	m.rtMu.Lock()
	for id := range want {
		victims = append(victims, m.runtimes[id]...)
		delete(m.runtimes, id)
	}
	m.rtMu.Unlock()
	for _, rt := range victims {
		if err := rt.Close(); err != nil {
			m.logf("关闭运行时失败: %v", err)
		}
	}
}

func (m *manager) statsFor(sessionID string, fallback Stats) Stats {
	m.rtMu.Lock()
	list := m.runtimes[sessionID]
	var last Runtime
	if len(list) > 0 {
		last = list[len(list)-1]
	}
	m.rtMu.Unlock()
	if last == nil {
		return fallback
	}
	return last.Stats()
}

// ── 能力探测 ────────────────────────────────────────────────────

func (m *manager) Capabilities(ctx context.Context) (Capabilities, error) {
	if err := m.ensureOpen(); err != nil {
		return Capabilities{}, err
	}
	if err := ctx.Err(); err != nil {
		return Capabilities{}, err
	}

	m.capMu.Lock()
	defer m.capMu.Unlock()
	if !m.capAt.IsZero() && time.Since(m.capAt) < capabilityTTL {
		return m.capCached, nil
	}

	caps := Capabilities{Platform: runtime.GOOS, MaxFPS: m.pol.MaxFPS}
	if c, err := m.sharedCapture(); err != nil {
		caps.Capture = Capability{
			Available: false,
			Reason:    err.Error(),
			Hint:      "确认系统已授予屏幕录制权限；Linux 需安装 gnome-screenshot/scrot/import 之一",
		}
	} else {
		caps.Capture = c.Info()
		if !caps.Capture.Available && caps.Capture.Hint == "" {
			// 后端只说「不可用」时，补一句用户能照做的话，否则前端只能显示一句报错。
			caps.Capture.Hint = "确认系统已授予屏幕录制权限；Linux 需安装 gnome-screenshot/scrot/import 之一"
		}
		if caps.Capture.Available {
			// 顺手抓一帧拿真实屏幕尺寸；抓不到不影响「抓屏可用」的结论。
			if f, err := c.Grab(ctx, GrabOptions{MaxWidth: m.pol.MaxWidth, Quality: m.pol.Quality}); err == nil {
				caps.Screen = Screen{Width: f.Width, Height: f.Height}
			}
		}
	}

	if in, err := m.sharedInput(); err != nil {
		caps.Input = Capability{
			Available: false,
			Reason:    err.Error(),
			Hint:      "macOS：系统设置 → 隐私与安全 → 辅助功能；Linux：安装 xdotool",
		}
	} else {
		caps.Input = in.Info()
		if !caps.Input.Available {
			if caps.Input.Reason == "" {
				caps.Input.Reason = "本机未授予辅助功能权限"
			}
			if caps.Input.Hint == "" {
				caps.Input.Hint = "macOS：系统设置 → 隐私与安全 → 辅助功能；Linux：安装 xdotool"
			}
		}
	}

	caps.Actions = actionsCapability()
	caps.Notice = capabilityNotice(caps)
	m.capCached, m.capAt = caps, time.Now()
	return caps, nil
}

func capabilityNotice(caps Capabilities) string {
	switch {
	case !caps.Capture.Available && !caps.Input.Available:
		return "本机缺少屏幕录制与辅助功能权限，远程桌面暂时不可用；快捷操作仍可使用"
	case !caps.Capture.Available:
		return "本机未授予屏幕录制权限，暂时看不到画面，只能使用快捷操作"
	case !caps.Input.Available:
		return "本机未授予辅助功能权限，只能看画面、不能接管鼠标键盘"
	default:
		return ""
	}
}

// ── 关闭 ────────────────────────────────────────────────────────

// Close 是幂等的收尾：停 TTL 协程 → 结束所有会话 → 关运行时/后端 → 刷盘。
// 顺序不能反：先结束会话（用户看到的是「已结束」而不是「还活着」），
// 再关抓屏泵，最后刷盘，保证磁盘记录的是终态。
func (m *manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		now := time.Now()
		for _, s := range m.sessions {
			if !isFinished(s.Status) {
				s.Status = StatusEnded
				s.EndedAt = &now
				s.EndReason = ReasonShutdown
			}
		}
		m.mu.Unlock()

		close(m.stop)
		m.wg.Wait()

		m.closeAllRuntimes()
		m.closeBackends()

		if err := m.saveSessions(); err != nil {
			m.closeErr = err
		}
		if err := m.saveDevices(); err != nil && m.closeErr == nil {
			m.closeErr = err
		}
	})
	return m.closeErr
}

func (m *manager) closeAllRuntimes() {
	m.rtMu.Lock()
	victims := make([]*trackedRuntime, 0, len(m.runtimes))
	for _, list := range m.runtimes {
		victims = append(victims, list...)
	}
	m.runtimes = make(map[string][]*trackedRuntime)
	m.rtMu.Unlock()
	for _, rt := range victims {
		if err := rt.Close(); err != nil {
			m.logf("关闭运行时失败: %v", err)
		}
	}
}

// ── 契约占位已删除 ──────────────────────────────────────────────
// newRuntime 已由 stream.go（R2）实现，本块按上面的约定整块移除。
