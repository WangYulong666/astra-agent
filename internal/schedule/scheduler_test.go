package schedule

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner 记录每次 CreateRun 的入参，可选地卡住（测租约）或报错（测失败路径）。
type fakeRunner struct {
	mu       sync.Mutex
	prompts  []string
	sessions []string
	err      error
	runID    string

	gate    chan struct{} // 非 nil：CreateRun 阻塞到它被关闭
	entered chan struct{} // 非 nil：进入 CreateRun 时非阻塞地发一个信号
}

func (f *fakeRunner) CreateRun(ctx context.Context, sessionID, prompt string) (string, error) {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.sessions = append(f.sessions, sessionID)
	err := f.err
	runID := f.runID
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return runID, nil
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeRunner) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func newTestScheduler(t *testing.T, cfg Config, fr Runner) (*fileStore, *scheduler, context.Context) {
	t.Helper()
	cfg.Enabled = true
	st, sched, err := Open(cfg, fr, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st.(*fileStore), sched.(*scheduler), context.Background()
}

// due 把一条 schedule 的 NextRunAt 拨到过去，模拟「到期了」。
func due(t *testing.T, st *fileStore, id string, at time.Time) {
	t.Helper()
	if _, err := st.mutate(context.Background(), id, func(cur *Schedule) error {
		cur.NextRunAt = at
		return nil
	}); err != nil {
		t.Fatalf("拨 NextRunAt: %v", err)
	}
}

func TestTickTriggersDueSchedule(t *testing.T) {
	fr := &fakeRunner{runID: "run-1"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	due(t, st, created.ID, now.Add(-time.Minute))

	n, err := sch.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 报错: %v", err)
	}
	if n != 1 {
		t.Fatalf("应触发 1 条，实际 %d", n)
	}
	if fr.count() != 1 {
		t.Fatalf("CreateRun 应被调用 1 次，实际 %d", fr.count())
	}
	if got := fr.lastPrompt(); got != created.Prompt {
		t.Fatalf("prompt 应原样透传，实际 %q", got)
	}
	fr.mu.Lock()
	sess := fr.sessions[0]
	fr.mu.Unlock()
	if sess != created.SessionID {
		t.Fatalf("session 应复用 schedule 绑定的，实际 %q", sess)
	}

	back, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.LastStatus != statusCreated {
		t.Fatalf("LastStatus 应为 created，实际 %q", back.LastStatus)
	}
	if back.LastRunID != "run-1" {
		t.Fatalf("LastRunID 应为 run-1，实际 %q", back.LastRunID)
	}
	if !back.LastRunAt.Equal(now) {
		t.Fatalf("LastRunAt 应为 %s，实际 %s", now, back.LastRunAt)
	}
	if !back.NextRunAt.After(now) {
		t.Fatalf("NextRunAt 必须往后推，实际 %s", back.NextRunAt)
	}
	wantNext := time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC) // */5 的下一个 5 分钟边界
	if !back.NextRunAt.Equal(wantNext) {
		t.Fatalf("NextRunAt 期望 %s，实际 %s", wantNext, back.NextRunAt)
	}

	// 幂等：同一个 now 再扫一次不该重复触发
	n, err = sch.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || fr.count() != 1 {
		t.Fatalf("同一时刻不该重复触发：n=%d calls=%d", n, fr.count())
	}

	// 时间走到下一个到期点才会再触发
	sch.now = func() time.Time { return back.NextRunAt }
	if n, _ := sch.Tick(ctx); n != 1 {
		t.Fatalf("到点后应再触发一次，实际 %d", n)
	}
}

func TestTickFailureAdvancesNextRunAt(t *testing.T) {
	fr := &fakeRunner{err: errors.New("沙箱起不来")}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	oldNext := now.Add(-time.Minute)
	due(t, st, created.ID, oldNext)

	n, err := sch.Tick(ctx)
	if err == nil {
		t.Fatal("CreateRun 失败时 Tick 应把错误报出来")
	}
	if !strings.Contains(err.Error(), "沙箱起不来") {
		t.Fatalf("错误应带上原因，实际 %v", err)
	}
	if n != 1 {
		t.Fatalf("抢到了就算触发过，n 应为 1，实际 %d", n)
	}
	if fr.count() != 1 {
		t.Fatalf("应尝试过 1 次，实际 %d", fr.count())
	}

	back, _ := st.Get(ctx, created.ID)
	if back.LastStatus != statusFailed {
		t.Fatalf("LastStatus 应为 failed，实际 %q", back.LastStatus)
	}
	if back.LastRunID != "" {
		t.Fatalf("失败时不该写 LastRunID，实际 %q", back.LastRunID)
	}
	if !back.NextRunAt.After(now) {
		t.Fatalf("失败也必须推进 NextRunAt，否则会每 Tick 重试刷爆日志：%s", back.NextRunAt)
	}

	// 再扫一次：不重试同一条
	if n, _ := sch.Tick(ctx); n != 0 || fr.count() != 1 {
		t.Fatalf("失败后不该立刻重试：n=%d calls=%d", n, fr.count())
	}
}

func TestTickSkipsWebhookSchedules(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	s := sampleCron("t1")
	s.Trigger = TriggerWebhook
	s.Cron = ""
	created, err := st.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	// 就算被人手改出一个过去的 NextRunAt，也不该被时间循环触发
	due(t, st, created.ID, now.Add(-time.Hour))

	if n, _ := sch.Tick(ctx); n != 0 {
		t.Fatalf("webhook 型不该按时间触发，实际 %d", n)
	}
	if fr.count() != 0 {
		t.Fatalf("webhook 型不该调 CreateRun，实际 %d 次", fr.count())
	}
}

func TestTickLeasePreventsOverlap(t *testing.T) {
	fr := &fakeRunner{
		runID:   "r",
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour, LeaseTTL: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	due(t, st, created.ID, now.Add(-time.Minute))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sch.Tick(ctx)
	}()
	<-fr.entered // 第一条已经卡在 CreateRun 里

	// 租约期内：即使 NextRunAt 又被拨回到过去（模拟别的实例/时钟抖动），也不重复触发
	due(t, st, created.ID, now.Add(-time.Minute))
	if n, _ := sch.Tick(ctx); n != 0 {
		t.Fatalf("租约期内不该重复触发，实际 %d", n)
	}
	close(fr.gate)
	<-done
	if fr.count() != 1 {
		t.Fatalf("应只触发一次，实际 %d", fr.count())
	}
}

func TestTickSkipsWhenClaimedByAnother(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour, LeaseTTL: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Minute)
	due(t, st, created.ID, past)

	// 先由「另一个实例」抢到（NextRunAt 已被推进）
	if ok, err := st.claim(ctx, created.ID, past, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("预置 claim 失败: %v / %v", ok, err)
	}
	// 本实例手里的快照仍是旧的，claim 必须失败，不能触发第二次
	stale := created
	stale.NextRunAt = past
	if fired, err := sch.fire(ctx, stale, now); err != nil || fired {
		t.Fatalf("别人抢到后不该再触发: fired=%v err=%v", fired, err)
	}
	if fr.count() != 0 {
		t.Fatalf("不该调用 CreateRun，实际 %d 次", fr.count())
	}
}

func TestRunNowDoesNotMoveNextRunAt(t *testing.T) {
	fr := &fakeRunner{runID: "run-now"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour}, fr)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sch.now = func() time.Time { return now }

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Get(ctx, created.ID)

	runID, err := sch.RunNow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-now" {
		t.Fatalf("runID 不对: %q", runID)
	}
	after, _ := st.Get(ctx, created.ID)
	if !after.NextRunAt.Equal(before.NextRunAt) {
		t.Fatalf("RunNow 不该动 NextRunAt：%s → %s", before.NextRunAt, after.NextRunAt)
	}
	if after.LastRunID != "run-now" || after.LastStatus != statusCreated {
		t.Fatalf("RunNow 应回写状态：%+v", after)
	}
	if !after.LastRunAt.Equal(now) {
		t.Fatalf("LastRunAt 应为 %s，实际 %s", now, after.LastRunAt)
	}
	if fr.count() != 1 {
		t.Fatalf("应调用一次 CreateRun，实际 %d", fr.count())
	}

	// 未知 ID
	if _, err := sch.RunNow(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 ID 应 ErrNotFound，实际 %v", err)
	}
}

func TestRunNowWithPromptAppendsPayload(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: time.Hour}, fr)
	s := sampleCron("t1")
	s.Trigger = TriggerWebhook
	s.Cron = ""
	created, err := st.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sch.RunNowWithPrompt(ctx, created.ID, `{"event":"push"}`); err != nil {
		t.Fatal(err)
	}
	got := fr.lastPrompt()
	if !strings.Contains(got, created.Prompt) || !strings.Contains(got, `{"event":"push"}`) {
		t.Fatalf("payload 应拼在 prompt 后面，实际 %q", got)
	}
	// 不写回存储：模板不该被一次 hook 的载荷污染
	back, _ := st.Get(ctx, created.ID)
	if back.Prompt != created.Prompt {
		t.Fatalf("prompt 模板被改写了: %q", back.Prompt)
	}
}

// fakeClock 每次读都往前走一分钟，让后台循环快速跨过 cron 的分钟边界。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.t
	c.t = c.t.Add(time.Minute)
	return v
}

func TestStartLoopTriggersAndStopStopsIt(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	st, sch, ctx := newTestScheduler(t, Config{Enabled: true, Tick: 2 * time.Millisecond, LeaseTTL: 50 * time.Millisecond}, fr)
	clk := &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	sch.now = clk.Now

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	due(t, st, created.ID, clk.t.Add(-time.Minute))

	sch.Start()
	sch.Start() // 重复调用无副作用

	deadline := time.Now().Add(3 * time.Second)
	for fr.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fr.count() < 2 {
		t.Fatalf("后台循环没跑起来，实际触发 %d 次", fr.count())
	}

	sch.Stop()
	sch.Stop() // 重复 Stop 不阻塞、不 panic

	n := fr.count()
	time.Sleep(80 * time.Millisecond)
	if got := fr.count(); got != n {
		t.Fatalf("Stop 之后还在触发：%d → %d", n, got)
	}
}

func TestStopWithoutStartDoesNotHang(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	_, sch, _ := newTestScheduler(t, Config{Enabled: true, Tick: time.Second}, fr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sch.Stop()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 在没 Start 的情况下挂住了")
	}
	sch.Start() // Stop 之后 Start 应为空操作，不该重新起循环
	time.Sleep(20 * time.Millisecond)
	if fr.count() != 0 {
		t.Fatalf("Stop 之后的 Start 不该再触发，实际 %d 次", fr.count())
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	if _, _, err := Open(Config{}, &fakeRunner{}, t.TempDir()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("未启用应 ErrDisabled，实际 %v", err)
	}
	if _, _, err := Open(Config{Enabled: true}, nil, t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("缺 runner 应 ErrInvalid，实际 %v", err)
	}
	if _, _, err := Open(Config{Enabled: true}, &fakeRunner{}, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空 dataDir 应 ErrInvalid，实际 %v", err)
	}
}

func TestOpenAppliesDefaults(t *testing.T) {
	st, sched, err := Open(Config{Enabled: true}, &fakeRunner{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sch := sched.(*scheduler)
	if sch.tick != 30*time.Second {
		t.Fatalf("Tick 默认应为 30s，实际 %s", sch.tick)
	}
	if sch.lease != 90*time.Second {
		t.Fatalf("LeaseTTL 默认应为 90s，实际 %s", sch.lease)
	}
	if st.(*fileStore).max != 50 {
		t.Fatalf("MaxPerTenant 默认应为 50，实际 %d", st.(*fileStore).max)
	}
}

func TestOpenRaisesTooSmallLease(t *testing.T) {
	_, sched, err := Open(Config{Enabled: true, Tick: time.Minute, LeaseTTL: time.Second}, &fakeRunner{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := sched.(*scheduler).lease; got < 3*time.Minute {
		t.Fatalf("过小的 LeaseTTL 应被抬到 3*Tick，实际 %s", got)
	}
}
