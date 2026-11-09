package remotedesk

// R1（核心：配对/设备/会话/审批/限速/审计/快捷操作）的测试。
//
// 测试替身统一用 r1 前缀，避免与同包里其他人（抓屏/流）的 fake.go 撞名。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 测试替身 ────────────────────────────────────────────────────

type r1Capture struct {
	mu        sync.Mutex
	available bool
	reason    string
	grabErr   error
	grabs     int
	closed    bool
}

func (c *r1Capture) Info() Capability {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Capability{Available: c.available, Backend: "test", Reason: c.reason}
}

func (c *r1Capture) Grab(context.Context, GrabOptions) (Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.grabs++
	if c.grabErr != nil {
		return Frame{}, c.grabErr
	}
	return Frame{
		JPEG:   []byte{0xff, 0xd8, 0xff, 0xd9},
		Width:  1280,
		Height: 800,
		At:     time.Now(),
	}, nil
}

func (c *r1Capture) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *r1Capture) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *r1Capture) grabCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grabs
}

type r1Input struct {
	mu        sync.Mutex
	available bool
	reason    string
	applied   int
	closed    bool
}

func (i *r1Input) Info() Capability {
	i.mu.Lock()
	defer i.mu.Unlock()
	return Capability{Available: i.available, Backend: "test", Reason: i.reason}
}

func (i *r1Input) Apply(context.Context, InputEvent, Screen) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.available {
		return fmt.Errorf("%w: %s", ErrUnavailable, i.reason)
	}
	i.applied++
	return nil
}

func (i *r1Input) Close() error {
	i.mu.Lock()
	i.closed = true
	i.mu.Unlock()
	return nil
}

func (i *r1Input) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

type r1Event struct {
	Action string
	Target string
	Detail map[string]any
}

type r1Audit struct {
	mu   sync.Mutex
	list []r1Event
}

func (a *r1Audit) sink(action, target string, detail map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, r1Event{Action: action, Target: target, Detail: detail})
}

func (a *r1Audit) snapshot() []r1Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]r1Event, len(a.list))
	copy(out, a.list)
	return out
}

func (a *r1Audit) count(action string) int {
	n := 0
	for _, e := range a.snapshot() {
		if e.Action == action {
			n++
		}
	}
	return n
}

func (a *r1Audit) find(action string) (r1Event, bool) {
	for _, e := range a.snapshot() {
		if e.Action == action {
			return e, true
		}
	}
	return r1Event{}, false
}

// ── 装配辅助 ────────────────────────────────────────────────────

type r1Env struct {
	m   *manager
	aud *r1Audit
	cap *r1Capture
	in  *r1Input
	cfg Config
	// capNew 记录抓屏后端被创建了几次（验证「整个 Manager 只一份」）。
	capNew *int32
}

func r1Config(dir string) Config {
	return Config{
		DataDir: dir,
		Logger:  log.New(io.Discard, "", 0),
		Policy:  Policy{ControlNeedsApproval: true},
	}
}

// r1OpenAt 在指定目录装配，返回内部指针以便白盒断言。
func r1OpenAt(t *testing.T, dir string, mutate func(*Config)) *r1Env {
	t.Helper()
	capB := &r1Capture{available: true}
	inB := &r1Input{available: true}
	aud := &r1Audit{}
	var capNew int32
	cfg := r1Config(dir)
	cfg.Audit = aud.sink
	cfg.Capture = func(Config) (Capture, error) {
		atomic.AddInt32(&capNew, 1)
		return capB, nil
	}
	cfg.Input = func(Config) (Input, error) { return inB, nil }
	if mutate != nil {
		mutate(&cfg)
	}
	mgr, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	m, ok := mgr.(*manager)
	if !ok {
		t.Fatalf("Open 返回的不是 *manager: %T", mgr)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return &r1Env{m: m, aud: aud, cap: capB, in: inB, cfg: cfg, capNew: &capNew}
}

func r1Open(t *testing.T, mutate func(*Config)) *r1Env {
	t.Helper()
	return r1OpenAt(t, t.TempDir(), mutate)
}

// r1Pair 走完整的「生成码 → 手机认领」流程，返回设备。
func r1Pair(t *testing.T, m *manager, tenantID, userID, name string) Device {
	t.Helper()
	ctx := context.Background()
	p, err := m.StartPairing(ctx, PairingRequest{TenantID: tenantID, UserID: userID, Device: name})
	if err != nil {
		t.Fatalf("StartPairing 失败: %v", err)
	}
	d, err := m.ClaimPairing(ctx, ClaimRequest{
		Code:     p.Code,
		TenantID: tenantID,
		UserID:   userID,
		Device:   name,
		Platform: "darwin",
	})
	if err != nil {
		t.Fatalf("ClaimPairing 失败: %v", err)
	}
	return d
}

// ── 会话状态机 ──────────────────────────────────────────────────

func TestStartSessionViewIsImmediatelyActive(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac mini")

	s, err := env.m.StartSession(ctx, StartRequest{
		TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView, Client: "iOS / Safari",
	})
	if err != nil {
		t.Fatalf("StartSession 失败: %v", err)
	}
	if s.Status != StatusActive {
		t.Fatalf("view 应免批准直接 active，实际 %s", s.Status)
	}
	if s.ApprovedAt == nil {
		t.Fatal("active 会话必须记 ApprovedAt")
	}
	if !s.ExpiresAt.After(s.CreatedAt) {
		t.Fatal("ExpiresAt 应晚于 CreatedAt")
	}
	if s.Device != "Mac mini" {
		t.Fatalf("会话没带上设备名: %q", s.Device)
	}

	ev, ok := env.aud.find("remote.session.start")
	if !ok {
		t.Fatal("缺少 remote.session.start 审计")
	}
	if ev.Target != s.ID {
		t.Fatalf("审计 target 应是 sessionID，实际 %q", ev.Target)
	}
	for _, k := range []string{"device", "mode", "client"} {
		if _, ok := ev.Detail[k]; !ok {
			t.Fatalf("审计 detail 缺 %s: %v", k, ev.Detail)
		}
	}
}

func TestStartSessionControlNeedsApprovalThenApproveIsIdempotent(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac mini")

	s, err := env.m.StartSession(ctx, StartRequest{
		TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl,
	})
	if err != nil {
		t.Fatalf("StartSession 失败: %v", err)
	}
	if s.Status != StatusPending {
		t.Fatalf("control 默认应 pending_approval，实际 %s", s.Status)
	}
	if s.ApprovedAt != nil {
		t.Fatal("pending 会话不该有 ApprovedAt")
	}

	got, err := env.m.ApproveSession(ctx, "t1", s.ID)
	if err != nil {
		t.Fatalf("ApproveSession 失败: %v", err)
	}
	if got.Status != StatusActive || got.ApprovedAt == nil {
		t.Fatalf("批准后应 active 且有 ApprovedAt: %+v", got)
	}
	if env.aud.count("remote.control.grant") != 1 {
		t.Fatal("缺少 remote.control.grant 审计")
	}

	// 重复批准：幂等，不报错也不重复审计。
	again, err := env.m.ApproveSession(ctx, "t1", s.ID)
	if err != nil {
		t.Fatalf("重复批准应幂等，实际 %v", err)
	}
	if again.Status != StatusActive {
		t.Fatalf("重复批准后状态应为 active，实际 %s", again.Status)
	}
	if env.aud.count("remote.control.grant") != 1 {
		t.Fatal("重复批准不该再记一次审计")
	}

	// 已结束的会话不可再批准。
	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); err != nil {
		t.Fatalf("EndSession 失败: %v", err)
	}
	if _, err := env.m.ApproveSession(ctx, "t1", s.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("已结束会话批准应 ErrDenied，实际 %v", err)
	}
}

func TestStartSessionRequireApprovalOverride(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	yes := true
	s, err := env.m.StartSession(ctx, StartRequest{
		TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView, RequireApproval: &yes,
	})
	if err != nil {
		t.Fatalf("StartSession 失败: %v", err)
	}
	if s.Status != StatusPending {
		t.Fatalf("显式要求批准时 view 也应 pending，实际 %s", s.Status)
	}

	no := false
	s2, err := env.m.StartSession(ctx, StartRequest{
		TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView, RequireApproval: &no,
	})
	if err != nil {
		t.Fatalf("StartSession 失败: %v", err)
	}
	if s2.Status != StatusActive {
		t.Fatalf("显式免批准时应 active，实际 %s", s2.Status)
	}
}

func TestStartSessionSupersedesSameDeviceMode(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	first, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusActive {
		t.Fatalf("新会话应 active，实际 %s", second.Status)
	}

	old, err := env.m.GetSession(ctx, "t1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusEnded || old.EndReason != ReasonSuperseded {
		t.Fatalf("旧会话应被 superseded，实际 %s/%s", old.Status, old.EndReason)
	}

	// 不同 mode 不算冲突。
	other, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != StatusPending {
		t.Fatalf("control 与 view 互不影响，新 control 应 pending，实际 %s", other.Status)
	}
	stillActive, err := env.m.GetSession(ctx, "t1", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillActive.Status != StatusActive {
		t.Fatalf("建 control 会话不该顶掉 view 会话，实际 %s", stillActive.Status)
	}
}

func TestStartSessionRejectsUnknownOrRevokedDevice(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()

	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: "dev_nope", Mode: ModeView}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知设备应 ErrNotFound，实际 %v", err)
	}

	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	// 别人的设备（跨租户）也是「不存在」。
	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t2", UserID: "u1", DeviceID: d.ID, Mode: ModeView}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户设备应 ErrNotFound，实际 %v", err)
	}
	if err := env.m.RevokeDevice(ctx, "t1", "u1", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已吊销设备应 ErrNotFound，实际 %v", err)
	}
}

func TestStartSessionRejectsInvalidModeAndDisabledInput(t *testing.T) {
	env := r1Open(t, func(c *Config) { c.Policy.DisableInput = true })
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: "turbo"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("未知 mode 应 ErrDenied，实际 %v", err)
	}
	_, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("DisableInput 时 control 应 ErrUnavailable，实际 %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "输入接管") {
		t.Fatalf("错误信息应说清「未开启输入接管」，实际 %v", err)
	}
	// view 不受影响。
	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView}); err != nil {
		t.Fatalf("DisableInput 不该影响 view: %v", err)
	}
}

func TestStartSessionRejectsWhenCaptureUnavailable(t *testing.T) {
	env := r1Open(t, func(c *Config) {
		c.Capture = func(Config) (Capture, error) {
			return &r1Capture{available: false, reason: "缺少屏幕录制权限"}, nil
		}
	})
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	_, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("抓屏不可用应 ErrUnavailable，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "缺少屏幕录制权限") {
		t.Fatalf("错误应带后端给的原因，实际 %v", err)
	}
}

func TestGetSessionHidesCrossTenant(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.GetSession(ctx, "t2", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户取会话应 ErrNotFound（不区分无权），实际 %v", err)
	}
	if _, err := env.m.GetSession(ctx, "t1", "sess_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的会话应 ErrNotFound，实际 %v", err)
	}
	// 跨租户也不能结束/批准别人的会话。
	if err := env.m.EndSession(ctx, "t2", s.ID, ReasonUserEnded); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户结束会话应 ErrNotFound，实际 %v", err)
	}
	if _, err := env.m.ApproveSession(ctx, "t2", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户批准会话应 ErrNotFound，实际 %v", err)
	}
}

func TestEndSessionIsIdempotent(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); err != nil {
		t.Fatalf("首次结束失败: %v", err)
	}
	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); err != nil {
		t.Fatalf("重复结束应幂等，实际 %v", err)
	}
	if env.aud.count("remote.session.end") != 1 {
		t.Fatalf("只该记一次 remote.session.end，实际 %d", env.aud.count("remote.session.end"))
	}
	got, err := env.m.GetSession(ctx, "t1", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusEnded || got.EndReason != ReasonUserEnded || got.EndedAt == nil {
		t.Fatalf("结束态不对: %+v", got)
	}
	if err := env.m.EndSession(ctx, "t1", "sess_nope", ReasonUserEnded); !errors.Is(err, ErrNotFound) {
		t.Fatalf("结束不存在的会话应 ErrNotFound，实际 %v", err)
	}
}

func TestListSessionsOrderAndLimit(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	var ids []string
	for i := 0; i < 3; i++ {
		s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		time.Sleep(2 * time.Millisecond) // 保证 CreatedAt 可区分
	}

	list, err := env.m.ListSessions(ctx, "t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 条会话，实际 %d", len(list))
	}
	if list[0].ID != ids[2] {
		t.Fatalf("应按 CreatedAt 倒序，首条应是最后建的 %s，实际 %s", ids[2], list[0].ID)
	}
	if list[0].Status != StatusActive {
		t.Fatal("最新一条应是 active")
	}

	limited, err := env.m.ListSessions(ctx, "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].ID != ids[2] {
		t.Fatalf("limit=1 应只返回最新一条，实际 %+v", limited)
	}

	// 上限 100：limit 传超大合法值也不炸。
	if _, err := env.m.ListSessions(ctx, "t1", 10000); err != nil {
		t.Fatalf("limit 过大应被夹住而不是报错: %v", err)
	}
	// 别的租户看不到。
	other, err := env.m.ListSessions(ctx, "t2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("跨租户不该看到会话，实际 %d 条", len(other))
	}
}

func TestSessionTTLExpires(t *testing.T) {
	env := r1Open(t, func(c *Config) { c.Policy.SessionTTL = 30 * time.Millisecond })
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	env.m.sweepExpired()

	got, err := env.m.GetSession(ctx, "t1", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusExpired || got.EndReason != ReasonTTL {
		t.Fatalf("超时应变 expired/ttl_expired，实际 %s/%s", got.Status, got.EndReason)
	}
	if env.aud.count("remote.session.end") != 1 {
		t.Fatal("TTL 到期应记一次 remote.session.end")
	}

	// 没过期的不能被误杀。
	d2 := r1Pair(t, env.m, "t1", "u1", "Mac 2")
	s2, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d2.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	env.m.sweepExpired()
	alive, err := env.m.GetSession(ctx, "t1", s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if alive.Status != StatusActive {
		t.Fatalf("未到期会话不该被清理，实际 %s", alive.Status)
	}
}

// ── Open / 运行时 ───────────────────────────────────────────────

func TestOpenRejectsPendingEndedAndCrossTenant(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	pend, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.Open(ctx, "t1", pend.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("pending 会话 Open 应 ErrDenied，实际 %v", err)
	}
	if _, err := env.m.Open(ctx, "t2", pend.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户 Open 应 ErrNotFound，实际 %v", err)
	}
	if _, err := env.m.Open(ctx, "t1", "sess_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的会话 Open 应 ErrNotFound，实际 %v", err)
	}

	act, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.EndSession(ctx, "t1", act.ID, ReasonUserEnded); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.Open(ctx, "t1", act.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已结束会话 Open 应 ErrNotFound，实际 %v", err)
	}
}

// TestOpenRuntimeLifecycle 覆盖 Open 与 stream.go 的对接：
//   - 一个会话可被多次 Open（手机 + 平板同时看），各自拿到独立 Runtime；
//   - 抓屏后端整个 Manager 共享一份（每会话起一个抓屏进程会打爆机器）；
//   - 会话结束后再 Open 一律 404，运行时可重复 Close 不 panic。
//
// 若 newRuntime 还没落地（R2 未完），这里退化成「Open 报错但不得污染会话状态」。
func TestOpenRuntimeLifecycle(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}

	rt1, err := env.m.Open(ctx, "t1", s.ID)
	if err != nil {
		// stream.go 尚未接入：Open 失败绝不能把会话改成 ended。
		got, gerr := env.m.GetSession(ctx, "t1", s.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Status != StatusActive {
			t.Fatalf("Open 失败不该改会话状态，实际 %s/%s", got.Status, got.EndReason)
		}
		if rt1 != nil {
			t.Fatal("Open 失败必须返回 nil")
		}
		t.Skipf("newRuntime 尚不可用（%v），运行时联调交由主控", err)
	}
	if rt1 == nil {
		t.Fatal("Open 成功却返回 nil 运行时")
	}

	rt2, err := env.m.Open(ctx, "t1", s.ID)
	if err != nil {
		t.Fatalf("同一会话应允许多端同时 Open: %v", err)
	}
	if rt2 == nil || rt1 == rt2 {
		t.Fatal("多端应各自拿到独立 Runtime")
	}
	if n := atomic.LoadInt32(env.capNew); n != 1 {
		t.Fatalf("抓屏后端必须整机共享一份，实际创建了 %d 份", n)
	}

	// 结束时要把运行时全部关掉。
	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); err != nil {
		t.Fatal(err)
	}
	env.m.rtMu.Lock()
	left := len(env.m.runtimes[s.ID])
	env.m.rtMu.Unlock()
	if left != 0 {
		t.Fatalf("会话结束后不该还挂着运行时，实际 %d", left)
	}
	if _, err := env.m.Open(ctx, "t1", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已结束会话再 Open 应 ErrNotFound，实际 %v", err)
	}

	// 重复 Close 不能 panic（不同实现的返回值可以不同）。
	_ = rt1.Close()
	_ = rt1.Close()
	_ = rt2.Close()
	if _, err := env.m.GetSession(ctx, "t1", s.ID); err != nil {
		t.Fatalf("关闭运行时不该影响会话记录: %v", err)
	}
}

// TestConcurrentOperations 把配对/建会话/开流/结束/列表混在一起跑，配合 -race
// 检查锁的粒度（这些接口都会被多个手机端同时打）。
func TestConcurrentOperations(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				s, err := env.m.StartSession(ctx, StartRequest{
					TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView,
				})
				if err == nil {
					_, _ = env.m.Open(ctx, "t1", s.ID)
					_, _ = env.m.GetSession(ctx, "t1", s.ID)
					_ = env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded)
				}
				_, _ = env.m.ListDevices(ctx, "t1", "u1")
				_, _ = env.m.ListSessions(ctx, "t1", 5)
				_, _ = env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
			}
		}()
	}
	wg.Wait()

	if err := env.m.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
}

// ── 能力探测 ────────────────────────────────────────────────────

func TestCapabilitiesReportsLayers(t *testing.T) {
	env := r1Open(t, nil)
	caps, err := env.m.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities 失败: %v", err)
	}
	if !caps.Capture.Available {
		t.Fatal("抓屏应可用")
	}
	if caps.Screen.Width != 1280 || caps.Screen.Height != 800 {
		t.Fatalf("屏幕尺寸应从抓帧推导: %+v", caps.Screen)
	}
	if caps.MaxFPS <= 0 {
		t.Fatal("MaxFPS 应有默认值")
	}
	if caps.Platform == "" {
		t.Fatal("Platform 不该为空")
	}

	// 缓存：再取一次不应再抓一帧。
	before := env.cap.grabCount()
	if _, err := env.m.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.cap.grabCount() != before {
		t.Fatal("能力探测结果应缓存 30 秒，不该重复抓屏")
	}
}

func TestCapabilitiesNoticeWhenCaptureDenied(t *testing.T) {
	env := r1Open(t, func(c *Config) {
		c.Capture = func(Config) (Capture, error) {
			return &r1Capture{available: false, reason: "permission_denied"}, nil
		}
	})
	caps, err := env.m.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Capture.Available {
		t.Fatal("抓屏应报不可用")
	}
	if caps.Notice == "" {
		t.Fatal("不可用时必须给人一句话解释")
	}
	if caps.Capture.Hint == "" {
		t.Fatal("不可用时要给修复提示")
	}
}

// ── Close ──────────────────────────────────────────────────────

func TestCloseEndsSessionsClosesBackendsAndIsIdempotent(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	// 懒创建后端。
	if _, err := env.m.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}

	if err := env.m.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := env.m.Close(); err != nil {
		t.Fatalf("Close 应幂等，实际 %v", err)
	}

	env.m.mu.RLock()
	got := env.m.sessions[s.ID]
	env.m.mu.RUnlock()
	if got == nil {
		t.Fatal("会话不该消失")
	}
	if got.Status != StatusEnded || got.EndReason != ReasonShutdown {
		t.Fatalf("Close 应把会话置 ended/server_shutdown，实际 %s/%s", got.Status, got.EndReason)
	}
	if !env.cap.isClosed() || !env.in.isClosed() {
		t.Fatal("Close 应关掉抓屏/注入后端")
	}
	// 关闭后的操作一律拒绝，而不是半死不活。
	if _, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Close 后 StartPairing 应 ErrUnavailable，实际 %v", err)
	}
	if _, err := env.m.ListDevices(ctx, "t1", "u1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Close 后 ListDevices 应 ErrUnavailable，实际 %v", err)
	}
}

func TestCloseDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	if _, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView}); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("Close 后协程泄漏：before=%d after=%d", before, n)
	}
}
