package browser

// 浏览器会话管理：起 Chrome、限并发、按空闲回收、保证不留僵尸进程。
//
// 进程生命周期的约定（三条，缺一条就会漏进程）：
//  1. cmd.Wait 一定在 goroutine 里调用——不调是僵尸，同步调会卡住平台；
//  2. 每条退出路径（正常 Close / 启动超时 / 启动被取消 / 自己崩掉）都走到
//     killProcess + releaseDir，没有「半启动」的状态漏回上层；
//  3. 每实例一个 user-data-dir，Close 时删掉，cookie 天然隔离。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxSessions = 4
	defaultIdleTimeout = 30 * time.Minute
	// launchTimeout 是「从 Start 到 stderr 出现 DevTools 行」的上限。
	// headless Chrome 一般 200~800ms 就报，10s 已经很宽松。
	launchTimeout = 10 * time.Second
	// termGrace 是 SIGTERM 之后等它自己退的时间，过了就 SIGKILL。
	termGrace = 2 * time.Second
	// waitKillTimeout 是 SIGKILL 之后等 Wait 返回的上限，防止 CloseAll 永久卡住。
	waitKillTimeout = 5 * time.Second
	// probeTimeout 是查 /json/list 统计页数的超时，本机回环，够用。
	probeTimeout = 500 * time.Millisecond
)

// instance 是一个活着的 Chrome 进程。
//
// 不可变字段（启动后只读）：id/cdpURL/httpURL/dir/cmd/done/removeDir。
// 可变字段（tenantID…pages）由 manager.mu 保护。
type instance struct {
	id      string
	cdpURL  string
	httpURL string
	dir     string
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error

	removeDir bool
	dirOnce   sync.Once

	tenantID  string
	userID    string
	sessionID string
	runID     string
	startedAt time.Time
	lastUsed  time.Time
	pages     int
}

func (i *instance) snapshot() Session {
	return Session{
		ID:        i.id,
		TenantID:  i.tenantID,
		UserID:    i.userID,
		SessionID: i.sessionID,
		RunID:     i.runID,
		CDPURL:    i.cdpURL,
		HTTPURL:   i.httpURL,
		StartedAt: i.startedAt,
		LastUsed:  i.lastUsed,
		Pages:     i.pages,
	}
}

// releaseDir 删掉本实例独占的 user-data-dir（幂等）。
// 持久 profile（Profile 非空）不在此列，多次启动共用同一目录。
func (i *instance) releaseDir() {
	if i == nil || !i.removeDir || i.dir == "" {
		return
	}
	i.dirOnce.Do(func() { _ = os.RemoveAll(i.dir) })
}

// killProcess 先 SIGTERM 后 SIGKILL，并等到进程真的被回收。
//
// 为什么不一上来就 SIGKILL：SIGTERM 让 Chrome 走正常退出流程，profile 目录里的
// 锁文件会被清掉；直接 KILL 有时留下 SingletonLock，持久 profile 会踩到。
func killProcess(i *instance) {
	if i == nil || i.cmd == nil || i.cmd.Process == nil {
		return
	}
	select {
	case <-i.done: // 已经退了，不用再打信号
		return
	default:
	}
	// Windows 上 Signal(SIGTERM) 不支持，会立刻报错 —— 那就直接 Kill。
	if err := i.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = i.cmd.Process.Kill()
	}
	select {
	case <-i.done:
		return
	case <-time.After(termGrace):
	}
	_ = i.cmd.Process.Kill()
	select {
	case <-i.done:
	case <-time.After(waitKillTimeout):
		// 连 SIGKILL 都收不回来只可能是内核异常，继续等会把平台退出流程拖死。
		// 此时进程已是 KILL 状态，不会长命。
	}
}

// manager 实现 Manager。
type manager struct {
	mu    sync.Mutex
	bin   string
	cfg   Config
	insts map[string]*instance
	// pending 是「正在启动、还没进 insts」的数量。并发 Launch 靠它抢车位，
	// 否则两个同时启动的实例能一起越过 MaxSessions。
	pending int
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
	reaper   sync.WaitGroup
}

// Open 探测 Chrome 并返回管理器。
//
// 找不到 Chrome 时返回包着 ErrDisabled 的错误：app 层把它降级成 nil，
// 平台照常启动（没装浏览器的机器也得能跑）。
func Open(cfg Config) (Manager, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	bin, err := locateChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	m := &manager{
		bin:   bin,
		cfg:   cfg,
		insts: make(map[string]*instance, cfg.MaxSessions),
		stop:  make(chan struct{}),
	}
	m.reaper.Add(1)
	go m.reapLoop()
	return m, nil
}

func (m *manager) Enabled() bool { return m != nil }

// Launch 起一个浏览器实例。
func (m *manager) Launch(ctx context.Context, req LaunchRequest) (Session, error) {
	if m == nil {
		return Session{}, ErrDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}

	rules, err := hostResolverRules(req.AllowedHosts)
	if err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Session{}, ErrDisabled
	}
	if len(m.insts)+m.pending >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return Session{}, ErrBusy
	}
	m.pending++
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.pending--
		m.mu.Unlock()
	}()

	dir, remove, err := profileDir(req)
	if err != nil {
		return Session{}, err
	}
	inst, err := startChrome(ctx, m.bin, dir, remove, m.cfg.Headless, rules)
	if err != nil {
		return Session{}, err
	}

	now := time.Now()
	inst.id = newSessionID()
	inst.tenantID, inst.userID = req.TenantID, req.UserID
	inst.sessionID, inst.runID = req.SessionID, req.RunID
	inst.startedAt, inst.lastUsed = now, now
	inst.pages = 1 // about:blank

	m.mu.Lock()
	if m.closed { // 启动期间平台被关了：不留孤儿
		m.mu.Unlock()
		killProcess(inst)
		inst.releaseDir()
		return Session{}, ErrDisabled
	}
	m.insts[inst.id] = inst
	out := inst.snapshot()
	m.mu.Unlock()

	go m.watch(inst)
	return out, nil
}

// watch 处理「Chrome 自己死了」：摘登记表 + 删目录。
// 主动 Close 也走 releaseDir，靠 once 保证不会重复删。
func (m *manager) watch(inst *instance) {
	<-inst.done
	m.retire(inst)
}

// retire 把实例从登记表里摘掉。
func (m *manager) retire(inst *instance) {
	if inst == nil {
		return
	}
	m.mu.Lock()
	if inst.id != "" && m.insts[inst.id] == inst {
		delete(m.insts, inst.id)
	}
	m.mu.Unlock()
	inst.releaseDir()
}

func (m *manager) Get(id string) (Session, bool) {
	if m == nil {
		return Session{}, false
	}
	m.mu.Lock()
	inst, ok := m.insts[id]
	if !ok {
		m.mu.Unlock()
		return Session{}, false
	}
	inst.lastUsed = time.Now() // 空闲计时从「最后一次 Get」重新开始
	out := inst.snapshot()
	m.mu.Unlock()

	if n, ok := livePages(out.HTTPURL); ok {
		out.Pages = n
	}
	return out, true
}

func (m *manager) List() []Session {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]Session, 0, len(m.insts))
	for _, inst := range m.insts {
		out = append(out, inst.snapshot())
	}
	m.mu.Unlock()

	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.Before(out[b].StartedAt) })
	for i := range out {
		if n, ok := livePages(out[i].HTTPURL); ok {
			out[i].Pages = n
		}
	}
	return out
}

// Close 关掉一个实例：kill 进程（SIGTERM → 2s → SIGKILL）+ 删 user-data-dir。
func (m *manager) Close(id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	inst, ok := m.insts[id]
	if ok {
		delete(m.insts, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	killProcess(inst)
	inst.releaseDir()
	return nil
}

// CloseAll 关掉所有实例，平台退出时调用。会等到所有子进程被回收才返回——
// 平台退出后还挂着一堆 headless Chrome 是运维最讨厌的形态。
func (m *manager) CloseAll() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() { close(m.stop) })

	m.mu.Lock()
	m.closed = true
	victims := make([]*instance, 0, len(m.insts))
	for _, inst := range m.insts {
		victims = append(victims, inst)
	}
	m.insts = make(map[string]*instance)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, inst := range victims {
		wg.Add(1)
		go func(i *instance) {
			defer wg.Done()
			killProcess(i)
			i.releaseDir()
		}(inst)
	}
	wg.Wait()
	m.reaper.Wait() // 等回收协程退出（它手里的实例目录也已删过）
}

// reapLoop 定期扫空闲实例。tick 取 IdleTimeout/4，夹在 [1s, 1min]：
// 太密没意义，太疏会让「30 分钟」变成「35 分钟」。
func (m *manager) reapLoop() {
	defer m.reaper.Done()
	tick := m.cfg.IdleTimeout / 4
	if tick < time.Second {
		tick = time.Second
	}
	if tick > time.Minute {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reapIdle(time.Now())
		}
	}
}

// reapIdle 关掉超过 IdleTimeout 没被 Get 过的实例。
// 抽成独立方法是为了能直接喂一个未来时间点做单测，不用真等 30 分钟。
func (m *manager) reapIdle(now time.Time) {
	var victims []*instance
	m.mu.Lock()
	for id, inst := range m.insts {
		if now.Sub(inst.lastUsed) >= m.cfg.IdleTimeout {
			delete(m.insts, id)
			victims = append(victims, inst)
		}
	}
	m.mu.Unlock()

	// 串行收：空闲回收不赶时间，串行还能避免同时打一堆 SIGTERM。
	for _, inst := range victims {
		killProcess(inst)
		inst.releaseDir()
	}
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("brw_%d", time.Now().UnixNano())
	}
	return "brw_" + hex.EncodeToString(b[:])
}

// livePages 查 /json/list 数真实页签数。失败（实例正在关、端口已断）返回 false，
// 调用方保留快照值——这里不该把 Get/List 变成会报错的路径。
func livePages(httpURL string) (int, bool) {
	if httpURL == "" {
		return 0, false
	}
	c := &http.Client{Timeout: probeTimeout}
	resp, err := c.Get(httpURL + "/json/list")
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var targets []struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return 0, false
	}
	n := 0
	for _, t := range targets {
		if t.Type == "page" {
			n++
		}
	}
	return n, true
}
