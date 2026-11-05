package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 假实现 ───────────────────────────────────────────────────

// fakeProvider 是内存版 sandbox.Provider：只实现回收器用到的 List / Destroy。
// Create / Acquire 直接失败——回收器一旦碰它们就是 bug，早点暴露比默默通过好。
type fakeProvider struct {
	mu      sync.Mutex
	infos   []sandbox.Info
	gone    map[string]bool // 已销毁的 id：List 不再返回，贴近真实 provider
	failFor map[string]error
	destroy []string
	listErr error
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Create(context.Context, sandbox.CreateRequest) (sandbox.Sandbox, error) {
	return nil, errors.New("fake: Create 不该被回收器调用")
}

func (p *fakeProvider) Acquire(context.Context, string) (sandbox.Sandbox, error) {
	return nil, errors.New("fake: Acquire 不该被回收器调用")
}

func (p *fakeProvider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failFor[id]; err != nil {
		return err
	}
	p.destroy = append(p.destroy, id)
	if p.gone == nil {
		p.gone = map[string]bool{}
	}
	p.gone[id] = true
	return nil
}

func (p *fakeProvider) List(context.Context) ([]sandbox.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := make([]sandbox.Info, 0, len(p.infos))
	for _, in := range p.infos {
		if p.gone[in.ID] {
			continue
		}
		out = append(out, in)
	}
	return out, nil
}

func (p *fakeProvider) destroyed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.destroy...)
}

// fakeStore 是内存版 store.Store。
//
// run / event 相关方法不实现真实逻辑（返回零值）：回收器只关心会话表，
// 在这里维护一套完整的事件流只会让测试更长，却测不到任何新东西。
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]store.Session

	deleteCalls int
	listErr     error
}

func newFakeStore(sessions ...store.Session) *fakeStore {
	m := make(map[string]store.Session, len(sessions))
	for _, s := range sessions {
		m[s.ID] = s
	}
	return &fakeStore{sessions: m}
}

func (s *fakeStore) CreateSession(_ context.Context, sess store.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	return nil
}

func (s *fakeStore) GetSession(_ context.Context, id string) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return store.Session{}, store.ErrNotFound
	}
	return sess, nil
}

func (s *fakeStore) ListSessions(context.Context) ([]store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]store.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out, nil
}

func (s *fakeStore) PutSession(_ context.Context, sess store.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	return nil
}

// DeleteSession 只记账不改数据：测试要断言「回收器从未调用它」，
// 真删了反而看不出调用发生过。
func (s *fakeStore) DeleteSession(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	return nil
}

func (s *fakeStore) DeleteCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

func (s *fakeStore) CreateRun(context.Context, store.Run) error { return nil }
func (s *fakeStore) GetRun(context.Context, string) (store.Run, error) {
	return store.Run{}, store.ErrNotFound
}
func (s *fakeStore) ListRuns(context.Context, string) ([]store.Run, error) { return nil, nil }
func (s *fakeStore) UpdateRun(context.Context, store.Run) error            { return nil }
func (s *fakeStore) Append(context.Context, string, store.NewEvent) (store.Event, error) {
	return store.Event{}, nil
}
func (s *fakeStore) Events(context.Context, string, int64, int) ([]store.Event, error) {
	return nil, nil
}
func (s *fakeStore) Close() error { return nil }

// ── 辅助 ─────────────────────────────────────────────────────

// fixedNow 固定「现在」，让所有阈值判断可以精确到秒断言。
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newReaper(p Policy, prov sandbox.Provider, st store.Store) *Reaper {
	r := New(p, Deps{Provider: prov, Store: st})
	r.Now = func() time.Time { return fixedNow }
	return r
}

func info(id string, age time.Duration) sandbox.Info {
	return sandbox.Info{
		ID:        id,
		Provider:  "fake",
		State:     sandbox.StateRunning,
		CreatedAt: fixedNow.Add(-age),
	}
}

func session(id, tenant, status string, idle time.Duration) store.Session {
	return store.Session{
		ID:           id,
		TenantID:     tenant,
		SandboxID:    id,
		Status:       status,
		CreatedAt:    fixedNow.Add(-idle),
		LastActiveAt: fixedNow.Add(-idle),
	}
}

// ── 回收 ─────────────────────────────────────────────────────

// 孤儿被销毁、有主的活跃沙箱不动。这是 D9 的第一条判据，也是最常见的残留来源：
// 用户直接关标签页、进程崩溃，store 里根本没有对应会话。
func TestSweepDestroysOrphanKeepsLive(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("orphan-1", time.Minute),
		info("live-1", time.Minute),
	}}
	st := newFakeStore(session("live-1", "t1", store.SessionActive, time.Minute))

	rep, err := newReaper(Policy{SweepInterval: time.Minute}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}

	if got := prov.destroyed(); len(got) != 1 || got[0] != "orphan-1" {
		t.Fatalf("期望只销毁 orphan-1，实际 %v", got)
	}
	if rep.Scanned != 2 || rep.Destroyed != 1 || rep.Orphaned != 1 || rep.Kept != 1 {
		t.Fatalf("报告不符: %+v", rep)
	}
	if rep.StartedAt.IsZero() || rep.FinishedAt.Before(rep.StartedAt) {
		t.Fatalf("时间戳没填: %+v", rep)
	}
}

// 超龄（MaxAge）与活跃度无关：只要活得够久就回收。
func TestSweepDestroysExpired(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("old-1", 2*time.Hour),
		info("fresh-1", 5*time.Minute),
	}}
	st := newFakeStore(
		session("old-1", "t1", store.SessionActive, time.Second), // 刚说过话，但仍超龄
		session("fresh-1", "t1", store.SessionActive, time.Minute),
	)

	rep, err := newReaper(Policy{MaxAge: time.Hour}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if got := prov.destroyed(); len(got) != 1 || got[0] != "old-1" {
		t.Fatalf("期望只销毁 old-1，实际 %v", got)
	}
	if rep.Expired != 1 || rep.Kept != 1 || rep.Orphaned != 0 {
		t.Fatalf("报告不符: %+v", rep)
	}
}

// 空闲（IdleTimeout）判据是 Session.LastActiveAt。
func TestSweepDestroysIdle(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("idle-1", 10*time.Minute),
		info("busy-1", 10*time.Minute),
	}}
	st := newFakeStore(
		session("idle-1", "t1", store.SessionActive, 31*time.Minute),
		session("busy-1", "t1", store.SessionActive, 2*time.Minute),
	)

	rep, err := newReaper(Policy{IdleTimeout: 30 * time.Minute}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if got := prov.destroyed(); len(got) != 1 || got[0] != "idle-1" {
		t.Fatalf("期望只销毁 idle-1，实际 %v", got)
	}
	if rep.Idle != 1 || rep.Kept != 1 {
		t.Fatalf("报告不符: %+v", rep)
	}
}

// 会话已非 active（deleted / idle 状态）也算孤儿：绑定关系失效，沙箱没有存在理由。
func TestSweepDestroysNonActiveSession(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("deleted-1", time.Minute),
		info("archived-1", time.Minute),
	}}
	st := newFakeStore(
		session("deleted-1", "t1", store.SessionDeleted, time.Minute),
		session("archived-1", "t1", store.SessionIdle, time.Minute),
	)

	rep, err := newReaper(Policy{SweepInterval: time.Minute}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if n := len(prov.destroyed()); n != 2 {
		t.Fatalf("期望销毁 2 个非 active 会话的沙箱，实际 %d: %v", n, prov.destroyed())
	}
	if rep.Orphaned != 2 || rep.Kept != 0 {
		t.Fatalf("报告不符: %+v", rep)
	}
}

// 全零 Policy = 关掉治理。绝不能退化成「没有会话就删」。
func TestSweepZeroPolicyDestroysNothing(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("orphan-1", 1000*time.Hour),
		info("deleted-1", 1000*time.Hour),
		info("live-1", 1000*time.Hour),
	}}
	// 连「会话已删」这种情况都给上：全零策略下也不该动。
	st := newFakeStore(
		session("deleted-1", "t1", store.SessionDeleted, 1000*time.Hour),
		session("live-1", "t1", store.SessionActive, 1000*time.Hour),
	)

	rep, err := newReaper(Policy{}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if got := prov.destroyed(); len(got) != 0 {
		t.Fatalf("零值策略不该销毁任何沙箱，实际 %v", got)
	}
	if rep.Destroyed != 0 || rep.Kept != 3 || rep.Scanned != 3 {
		t.Fatalf("报告不符: %+v", rep)
	}
}

// 单个 Destroy 失败不影响其它沙箱，错误进 Report.Errors，整轮不返回 error。
func TestSweepContinuesAfterDestroyFailure(t *testing.T) {
	prov := &fakeProvider{
		infos: []sandbox.Info{
			info("boom-1", time.Minute),
			info("orphan-2", time.Minute),
			info("orphan-3", time.Minute),
		},
		failFor: map[string]error{"boom-1": errors.New("runtime 挂了")},
	}
	st := newFakeStore()

	rep, err := newReaper(Policy{SweepInterval: time.Minute}, prov, st).Sweep(context.Background())
	if err != nil {
		t.Fatalf("单个沙箱失败不该让整轮报错: %v", err)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "boom-1") {
		t.Fatalf("期望一条关于 boom-1 的错误，实际 %v", rep.Errors)
	}
	got := prov.destroyed()
	if len(got) != 2 {
		t.Fatalf("期望其余两个仍被销毁，实际 %v", got)
	}
	if rep.Destroyed != 2 || rep.Orphaned != 2 {
		t.Fatalf("失败的沙箱不该计入 Destroyed: %+v", rep)
	}
}

// Destroy 返回 ErrNotFound 视为成功（目标状态已达成），不该进 Errors。
func TestSweepTreatsNotFoundAsDestroyed(t *testing.T) {
	prov := &fakeProvider{
		infos:   []sandbox.Info{info("gone-1", time.Minute)},
		failFor: map[string]error{"gone-1": sandbox.ErrNotFound},
	}

	rep, err := newReaper(Policy{SweepInterval: time.Minute}, prov, newFakeStore()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("ErrNotFound 不该记成错误: %v", rep.Errors)
	}
	if rep.Destroyed != 1 || rep.Orphaned != 1 {
		t.Fatalf("报告不符: %+v", rep)
	}
}

// 回收只销毁沙箱，绝不删会话数据（D9 + runner.DeleteSession 的顺序语义）。
func TestSweepNeverDeletesSessionData(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{
		info("orphan-1", time.Minute),
		info("deleted-1", time.Minute),
		info("old-1", 5*time.Hour),
		info("idle-1", time.Minute),
	}}
	st := newFakeStore(
		session("deleted-1", "t1", store.SessionDeleted, time.Minute),
		session("old-1", "t1", store.SessionActive, time.Minute),
		session("idle-1", "t1", store.SessionActive, 5*time.Hour),
	)
	r := newReaper(Policy{IdleTimeout: time.Hour, MaxAge: time.Hour, SweepInterval: time.Minute}, prov, st)

	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	if rep.Destroyed != 4 {
		t.Fatalf("期望四个都被回收，实际 %+v", rep)
	}
	if n := st.DeleteCalls(); n != 0 {
		t.Fatalf("回收器调用了 DeleteSession %d 次，这会销毁用户数据", n)
	}
	if _, err := st.GetSession(context.Background(), "deleted-1"); err != nil {
		t.Fatalf("会话记录不该被删: %v", err)
	}
}

// 枚举失败要返回 error，而不是返回一份「全 0 的成功报告」——
// 后者会让运维以为一切正常。
func TestSweepListError(t *testing.T) {
	prov := &fakeProvider{listErr: errors.New("provider 不可达")}
	rep, err := newReaper(Policy{SweepInterval: time.Minute}, prov, newFakeStore()).Sweep(context.Background())
	if err == nil {
		t.Fatal("枚举失败应返回 error")
	}
	if rep.FinishedAt.IsZero() {
		t.Fatal("失败也要填 FinishedAt，否则前端显示的时间会是零值")
	}
}

// LastReport 在扫描后可读，未扫描过时 ok=false。
func TestLastReport(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{info("orphan-1", time.Minute)}}
	r := newReaper(Policy{SweepInterval: time.Minute}, prov, newFakeStore())

	if _, ok := r.LastReport(); ok {
		t.Fatal("未扫描过时不该有报告")
	}
	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep 返回错误: %v", err)
	}
	rep, ok := r.LastReport()
	if !ok || rep.Destroyed != 1 {
		t.Fatalf("期望读到销毁 1 个的报告，实际 ok=%v %+v", ok, rep)
	}
}

// SweepInterval<=0 时 Run 必须立刻返回，不能空转（也避免 NewTicker(0) 的 panic）。
func TestRunWithoutIntervalReturnsImmediately(t *testing.T) {
	r := newReaper(Policy{}, &fakeProvider{}, newFakeStore())

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 在未配置周期时没有返回")
	}
}

// 后台循环：ctx 取消后退出，且期间确实扫过。
func TestRunStopsOnContextCancel(t *testing.T) {
	prov := &fakeProvider{infos: []sandbox.Info{info("orphan-1", time.Minute)}}
	r := newReaper(Policy{SweepInterval: 10 * time.Millisecond}, prov, newFakeStore())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	deadline := time.After(3 * time.Second)
	for {
		if _, ok := r.LastReport(); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("后台循环没有执行任何扫描")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 没有退出")
	}
}

// 手动 Sweep 与后台 Run 同时跑，不能出现竞态/重复销毁。
// 用 -race 跑这个用例才有意义：它同时压 mu 与 fake 的锁。
func TestConcurrentSweeps(t *testing.T) {
	infos := make([]sandbox.Info, 0, 20)
	for i := 0; i < 20; i++ {
		infos = append(infos, info("orphan-"+string(rune('a'+i)), time.Minute))
	}
	prov := &fakeProvider{infos: infos}
	r := newReaper(Policy{SweepInterval: time.Minute}, prov, newFakeStore())

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Sweep(context.Background())
			_, _ = r.LastReport()
		}()
	}
	wg.Wait()

	// 串行化之后，第一个 Sweep 清空，后面几轮 Scanned 仍是 20 但 Destroyed 为 0
	// （fake provider 的 List 返回固定切片，真实 provider 会真的变空——
	// 这里断言的是「每个 id 只被销毁一次」，那才是重复销毁的判据）。
	seen := map[string]int{}
	for _, id := range prov.destroyed() {
		seen[id]++
	}
	if len(seen) != len(infos) {
		t.Fatalf("期望 %d 个沙箱各销毁一次，实际 %v", len(infos), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s 被销毁 %d 次", id, n)
		}
	}
}

// ── 配额 ─────────────────────────────────────────────────────

func TestQuotaCheck(t *testing.T) {
	// 上限 2：两个 active 已占满。
	full := newFakeStore(
		session("s1", "t1", store.SessionActive, time.Minute),
		session("s2", "t1", store.SessionActive, time.Minute),
	)
	// 只剩一个：一个 active + 一个 deleted（deleted 不占额度）。
	room := newFakeStore(
		session("s1", "t1", store.SessionActive, time.Minute),
		session("s2", "t1", store.SessionDeleted, time.Minute),
	)
	// 别的租户占满了，与本租户无关。
	other := newFakeStore(
		session("a", "t2", store.SessionActive, time.Minute),
		session("b", "t2", store.SessionActive, time.Minute),
		session("c", "t2", store.SessionActive, time.Minute),
	)
	// 没有沙箱的会话（SandboxID 为空）不占额度。
	noSandbox := newFakeStore(store.Session{
		ID: "x", TenantID: "t1", Status: store.SessionActive,
	})

	cases := []struct {
		name    string
		store   *fakeStore
		max     int
		tenant  string
		wantErr bool
	}{
		{"正好到上限要拒绝", full, 2, "t1", true},
		{"未到上限放行", room, 3, "t1", false},
		{"别的租户已满不影响本租户", other, 3, "t1", false},
		{"MaxPerTenant=0 永远放行", full, 0, "t1", false},
		{"无沙箱的会话不占额度", noSandbox, 1, "t1", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &Quota{Store: c.store, MaxPerTenant: c.max}
			err := q.Check(context.Background(), c.tenant)
			if c.wantErr {
				if !errors.Is(err, ErrQuotaExceeded) {
					t.Fatalf("期望 ErrQuotaExceeded，实际 %v", err)
				}
				// 文案要能直接给前端展示：必须带「当前 N / 上限 M」。
				if msg := err.Error(); !strings.Contains(msg, "当前 2 / 上限 2") {
					t.Fatalf("错误信息缺少计数: %q", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该被拒绝: %v", err)
			}
		})
	}
}

// 统计会话失败必须冒泡成 error，不能「查不到就当没超限」放行。
func TestQuotaCheckStoreError(t *testing.T) {
	st := newFakeStore()
	st.listErr = errors.New("db 挂了")
	q := &Quota{Store: st, MaxPerTenant: 1}
	if err := q.Check(context.Background(), "t1"); err == nil {
		t.Fatal("存储失败应返回 error")
	}
}
