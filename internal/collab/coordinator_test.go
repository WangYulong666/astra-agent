package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── fake Sessions ──────────────────────────────────────────────
//
// 用真实 store.OpenFile 造父会话/父 run/事件；子会话与子 run 由 fake 落到
// 同一个 store 里（create session/run + append delta），这样 collab 读到的
// 全是真数据，只是「跑 agent」这一步被替换成直接置状态。

type fakeConfig struct {
	complete            bool             // StartRun 建的 run 直接是 completed
	deltas              []string         // 追加成 message_delta {"text": ...}
	extraEvents         []store.NewEvent // 追加任意事件，用来验证字段过滤
	cancelMakesTerminal bool             // CancelRun 是否把 run 置 cancelled
}

type fakeSessions struct {
	st store.Store

	mu sync.Mutex
	// 所有配置与计数器都在 mu 下读写，避免 -race 把测试自己的共享状态报错。
	cfg      fakeConfig
	seq      int
	created  []CreateOpts
	canceled []string
}

func (f *fakeSessions) configure(fn func(*fakeConfig)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.cfg)
}

func (f *fakeSessions) snapshot() (created []CreateOpts, canceled []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CreateOpts(nil), f.created...), append([]string(nil), f.canceled...)
}

func (f *fakeSessions) CreateSession(ctx context.Context, o CreateOpts) (SessionInfo, error) {
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("sess-%d", f.seq)
	f.created = append(f.created, o)
	f.mu.Unlock()

	now := time.Now().UTC()
	s := store.Session{
		ID: id, TenantID: o.TenantID, UserID: o.UserID,
		ParentSessionID: o.ParentSessionID, Status: store.SessionActive,
		CreatedAt: now, LastActiveAt: now,
	}
	if err := f.st.CreateSession(ctx, s); err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{ID: id}, nil
}

func (f *fakeSessions) StartRun(ctx context.Context, sid, input, parentRunID string) (RunInfo, error) {
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("run-%d", f.seq)
	cfg := f.cfg
	deltas := append([]string(nil), cfg.deltas...)
	extra := append([]store.NewEvent(nil), cfg.extraEvents...)
	f.mu.Unlock()

	status := store.RunRunning
	if cfg.complete {
		status = store.RunCompleted
	}
	r := store.Run{
		ID: id, SessionID: sid, Status: status, Input: input,
		CreatedAt: time.Now().UTC(), NextSeq: 1,
	}
	if err := f.st.CreateRun(ctx, r); err != nil {
		return RunInfo{}, err
	}
	// payload 结构照 runner/sink.go 的 flushLocked：map[string]any{"text": ...}
	for _, d := range deltas {
		if _, err := f.st.Append(ctx, id, store.NewEvent{
			Type:    store.EventMessageDelta,
			Payload: map[string]any{"text": d},
		}); err != nil {
			return RunInfo{}, err
		}
	}
	for _, e := range extra {
		if _, err := f.st.Append(ctx, id, e); err != nil {
			return RunInfo{}, err
		}
	}
	return RunInfo{ID: id}, nil
}

func (f *fakeSessions) CancelRun(ctx context.Context, runID string) error {
	f.mu.Lock()
	f.canceled = append(f.canceled, runID)
	makesTerminal := f.cfg.cancelMakesTerminal
	f.mu.Unlock()

	if !makesTerminal {
		return nil
	}
	r, err := f.st.GetRun(ctx, runID)
	if err != nil {
		return nil
	}
	r.Status = store.RunCancelled
	r.EndedAt = time.Now().UTC()
	return f.st.UpdateRun(ctx, r)
}

var _ Sessions = (*fakeSessions)(nil)

// ── 测试脚手架 ────────────────────────────────────────────────

type testEnv struct {
	dir  string
	st   *store.File
	fake *fakeSessions
	c    Coordinator
}

func newEnv(t *testing.T, tweak func(*Config)) *testEnv {
	t.Helper()
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	dataDir := filepath.Join(root, "collab")

	st, err := store.OpenFile(storeDir, store.Options{Sync: false})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSessions{st: st}
	fake.configure(func(c *fakeConfig) { c.complete = true })

	cfg := Config{
		Sessions: fake,
		DataDir:  dataDir,
		MaxDepth: 2,
		Timeout:  2 * time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := Open(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{dir: dataDir, st: st, fake: fake, c: c}
	t.Cleanup(func() {
		_ = c.Close()
		_ = st.Close()
	})
	return env
}

// ensureParentSession 造一个共享父会话（带与 req 故意不同的归属，用于验证
// TenantID/UserID 走 req 而不是从父会话里另猜一份）。
func (e *testEnv) ensureParentSession(t *testing.T, sid string) {
	t.Helper()
	if _, err := e.st.GetSession(context.Background(), sid); err == nil {
		return
	}
	now := time.Now().UTC()
	s := store.Session{
		ID: sid, TenantID: "t-parent", UserID: "u-parent",
		Status: store.SessionActive, CreatedAt: now, LastActiveAt: now,
	}
	if err := e.st.CreateSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) makeRun(t *testing.T, sid, runID, parentRunID string) {
	t.Helper()
	r := store.Run{
		ID: runID, SessionID: sid, ParentRunID: parentRunID,
		Status: store.RunRunning, Input: "parent input",
		CreatedAt: time.Now().UTC(), NextSeq: 1,
	}
	if err := e.st.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) readChildren(t *testing.T) []Child {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "children.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var items []Child
	if err := json.Unmarshal(b, &items); err != nil {
		t.Fatalf("children.json 无法解析: %v", err)
	}
	return items
}

func (e *testEnv) events(t *testing.T, runID string) []store.Event {
	t.Helper()
	evs, err := e.st.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// ── 用例 ──────────────────────────────────────────────────────

func TestSpawnHappyPath(t *testing.T) {
	env := newEnv(t, nil)
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")
	env.fake.configure(func(c *fakeConfig) { c.deltas = []string{"Hello ", "world", "!"} })

	child, err := env.c.Spawn(context.Background(), SpawnRequest{
		ParentRunID: "run-parent", Input: "do it", Title: "子任务",
		TenantID: "t-server", UserID: "u-server",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if child.Status != store.RunCompleted {
		t.Fatalf("status = %q, want completed", child.Status)
	}
	if child.Summary != "Hello world!" {
		t.Fatalf("summary = %q, want %q", child.Summary, "Hello world!")
	}
	if child.Depth != 1 {
		t.Fatalf("depth = %d, want 1", child.Depth)
	}
	if child.RunID == "" || child.SessionID == "" || child.ParentRunID != "run-parent" {
		t.Fatalf("记录字段不全: %+v", child)
	}
	if child.EndedAt.IsZero() {
		t.Fatal("EndedAt 未填")
	}

	// TenantID/UserID 必须用 req 的服务端值，不能是父会话的 t-parent/u-parent。
	created, _ := env.fake.snapshot()
	if len(created) != 1 {
		t.Fatalf("CreateSession 调用 %d 次", len(created))
	}
	if created[0].TenantID != "t-server" || created[0].UserID != "u-server" {
		t.Fatalf("归属错误: %+v", created[0])
	}
	if created[0].ParentSessionID != "parent-sess" || created[0].Title != "子任务" {
		t.Fatalf("CreateOpts 错误: %+v", created[0])
	}

	// 父 run 应追加两条事件，且 Spawned 在 Result 之前（append-only 才能看出间隔）。
	evs := env.events(t, "run-parent")
	var spawnedSeq, resultSeq int64
	var gotSpawned, gotResult bool
	for _, ev := range evs {
		switch ev.Type {
		case store.EventChildSpawned:
			gotSpawned = true
			spawnedSeq = ev.Seq
			var p spawnedPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.ChildRunID != child.RunID || p.ChildSessionID != child.SessionID ||
				p.Input != "do it" || p.Title != "子任务" {
				t.Fatalf("spawned payload 错误: %+v", p)
			}
		case store.EventChildResult:
			gotResult = true
			resultSeq = ev.Seq
			var p resultPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.ChildRunID != child.RunID || p.Status != store.RunCompleted ||
				p.Summary != "Hello world!" || p.Error != "" {
				t.Fatalf("result payload 错误: %+v", p)
			}
		}
	}
	if !gotSpawned || !gotResult {
		t.Fatalf("缺事件: spawned=%v result=%v (共 %d 条)", gotSpawned, gotResult, len(evs))
	}
	if spawnedSeq >= resultSeq {
		t.Fatalf("事件顺序错: spawned seq=%d, result seq=%d", spawnedSeq, resultSeq)
	}

	// children.json 落了最终状态。
	items := env.readChildren(t)
	if len(items) != 1 || items[0].Status != store.RunCompleted || items[0].Summary != "Hello world!" {
		t.Fatalf("children.json 错误: %+v", items)
	}
}

// 验证摘要只认 sink.go 里的真实字段 text，且按 rune 截断到 2000。
func TestSummaryTextFieldAndTruncation(t *testing.T) {
	env := newEnv(t, nil)
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")
	long := strings.Repeat("汉", 2500)
	env.fake.configure(func(c *fakeConfig) {
		c.deltas = []string{long}
		c.extraEvents = []store.NewEvent{
			{Type: store.EventMessageDelta, Payload: map[string]any{"content": "WRONG-FIELD"}},
			{Type: store.EventThoughtDelta, Payload: map[string]any{"text": "THOUGHT"}},
		}
	})

	child, err := env.c.Spawn(context.Background(), SpawnRequest{ParentRunID: "run-parent", Input: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(child.Summary)); got != summaryLimit {
		t.Fatalf("summary rune 数 = %d, want %d", got, summaryLimit)
	}
	for _, r := range child.Summary {
		if r != '汉' {
			t.Fatalf("混入了不该有的字符 %q", r)
		}
	}
	if strings.Contains(child.Summary, "WRONG") || strings.Contains(child.Summary, "THOUGHT") {
		t.Fatalf("把错误字段/非 message_delta 混进了摘要: %q", child.Summary)
	}
}

func TestSpawnTooDeep(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.MaxDepth = 2 })
	env.ensureParentSession(t, "parent-sess")
	// root(0) → child1(1) → child2(2)；从 child2 再派生应被拒。
	env.makeRun(t, "parent-sess", "run-root", "")
	env.makeRun(t, "parent-sess", "run-c1", "run-root")
	env.makeRun(t, "parent-sess", "run-c2", "run-c1")

	start := time.Now()
	_, err := env.c.Spawn(context.Background(), SpawnRequest{ParentRunID: "run-c2", Input: "x"})
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("err = %v, want ErrTooDeep", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("深度检查异常缓慢")
	}
	// 深度检查必须发生在任何创建动作之前，不留半截子会话。
	created, _ := env.fake.snapshot()
	if len(created) != 0 {
		t.Fatalf("超限后仍创建了 %d 个子会话", len(created))
	}
	if items := env.readChildren(t); len(items) != 0 {
		t.Fatalf("超限后仍落了记录: %+v", items)
	}
}

// 循环 ParentRunID 链必须防御性退出，不能死循环。
func TestSpawnCycleChain(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.MaxDepth = 2 })
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-a", "run-b")
	env.makeRun(t, "parent-sess", "run-b", "run-a")

	done := make(chan error, 1)
	go func() {
		_, err := env.c.Spawn(context.Background(), SpawnRequest{ParentRunID: "run-a", Input: "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTooDeep) {
			t.Fatalf("err = %v, want ErrTooDeep（环被 seen 截断）", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("循环链导致死循环")
	}
}

func TestSpawnTimeout(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.Timeout = 100 * time.Millisecond })
	env.fake.configure(func(c *fakeConfig) {
		c.complete = false            // 子 run 永远不终态
		c.cancelMakesTerminal = false // CancelRun 也不置终态
	})
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")

	// 外层 3s 看门狗：即使内部 timer 失灵也不会把测试挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	child, err := env.c.Spawn(ctx, SpawnRequest{ParentRunID: "run-parent", Input: "x"})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("超时耗时异常: %v", elapsed)
	}
	if child.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", child.Status)
	}
	_, canceled := env.fake.snapshot()
	if len(canceled) != 1 || canceled[0] != child.RunID {
		t.Fatalf("超时未 CancelRun: %v", canceled)
	}
	// 超时记录仍要落库，且要发 Result 事件。
	items := env.readChildren(t)
	if len(items) != 1 || items[0].Status != "timeout" {
		t.Fatalf("超时记录未落库: %+v", items)
	}
	var sawTimeoutResult bool
	for _, ev := range env.events(t, "run-parent") {
		if ev.Type == store.EventChildResult {
			var p resultPayload
			_ = json.Unmarshal(ev.Payload, &p)
			if p.Status == "timeout" {
				sawTimeoutResult = true
			}
		}
	}
	if !sawTimeoutResult {
		t.Fatal("超时未追加 EventChildResult")
	}
}

func TestSpawnContextCanceled(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.Timeout = 10 * time.Second })
	env.fake.configure(func(c *fakeConfig) { c.complete = false })
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消，确保是 ctx 而不是 timer 触发退出
	start := time.Now()
	child, err := env.c.Spawn(ctx, SpawnRequest{ParentRunID: "run-parent", Input: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ctx 取消后 Spawn 未及时返回")
	}
	if child.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", child.Status)
	}
	if items := env.readChildren(t); len(items) != 1 {
		t.Fatalf("ctx 取消后记录未落库: %+v", items)
	}
}

func TestConcurrentSpawn(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.Timeout = 5 * time.Second })
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := env.c.Spawn(context.Background(), SpawnRequest{
				ParentRunID: "run-parent", Input: fmt.Sprintf("task-%d", i),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发 Spawn: %v", err)
		}
	}

	// children.json 必须仍是合法 JSON，且条数正好 n、RunID 不重复。
	items := env.readChildren(t)
	if len(items) != n {
		t.Fatalf("children.json 条数 = %d, want %d", len(items), n)
	}
	seen := map[string]bool{}
	for _, ch := range items {
		if seen[ch.RunID] {
			t.Fatalf("重复 RunID %s", ch.RunID)
		}
		seen[ch.RunID] = true
		if ch.Status != store.RunCompleted {
			t.Fatalf("%s 状态 = %q", ch.RunID, ch.Status)
		}
	}
	var spawned, result int
	for _, ev := range env.events(t, "run-parent") {
		switch ev.Type {
		case store.EventChildSpawned:
			spawned++
		case store.EventChildResult:
			result++
		}
	}
	if spawned != n || result != n {
		t.Fatalf("父事件数 spawned=%d result=%d, want %d each", spawned, result, n)
	}
}

func TestListSortedAndMissingParent(t *testing.T) {
	env := newEnv(t, nil)
	cc := env.c.(*coordinator)
	base := time.Now().UTC().Add(-time.Hour)
	// 直接塞乱序的 CreatedAt，确定性地验证排序逻辑。
	cc.mu.Lock()
	for i, at := range []time.Time{base.Add(3 * time.Hour), base, base.Add(2 * time.Hour)} {
		ch := Child{RunID: fmt.Sprintf("r-%d", i), ParentRunID: "p", CreatedAt: at}
		cc.children[ch.RunID] = ch
	}
	cc.mu.Unlock()

	got, err := env.c.List(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List 条数 = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Fatalf("未按 CreatedAt 正序: %v 在 %v 之前", got[i].CreatedAt, got[i-1].CreatedAt)
		}
	}
	// 不存在的父 run 返回空列表而不是错误。
	empty, err := env.c.List(context.Background(), "no-such-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("不存在的父 run 应返回空列表, got %+v", empty)
	}
}

func TestCancel(t *testing.T) {
	env := newEnv(t, nil)
	// 未知 id。
	if err := env.c.Cancel(context.Background(), "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	cc := env.c.(*coordinator)
	cc.mu.Lock()
	cc.children["run-x"] = Child{
		RunID: "run-x", ParentRunID: "p", Status: "running",
		CreatedAt: time.Now().UTC(),
	}
	cc.mu.Unlock()

	if err := env.c.Cancel(context.Background(), "run-x"); err != nil {
		t.Fatal(err)
	}
	_, canceled := env.fake.snapshot()
	if len(canceled) != 1 || canceled[0] != "run-x" {
		t.Fatalf("未调 CancelRun: %v", canceled)
	}
	cc.mu.RLock()
	status := cc.children["run-x"].Status
	cc.mu.RUnlock()
	if status != "cancelled" {
		t.Fatalf("取消后 status = %q", status)
	}
	// 已非 running，再 Cancel 是 no-op，不重复调 CancelRun。
	if err := env.c.Cancel(context.Background(), "run-x"); err != nil {
		t.Fatal(err)
	}
	_, canceled2 := env.fake.snapshot()
	if len(canceled2) != 1 {
		t.Fatalf("非 running 却重复取消: %v", canceled2)
	}
}

func TestCloseNoLeak(t *testing.T) {
	env := newEnv(t, func(cfg *Config) { cfg.Timeout = 30 * time.Second })
	env.fake.configure(func(c *fakeConfig) { c.complete = false })
	env.ensureParentSession(t, "parent-sess")
	env.makeRun(t, "parent-sess", "run-parent", "")

	before := runtime.NumGoroutine()
	const n = 3
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = env.c.Spawn(context.Background(), SpawnRequest{ParentRunID: "run-parent", Input: "x"})
		}()
	}
	// 让它们进入 waitTerminal 的轮询。
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- env.c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 卡住：WaitGroup 泄漏")
	}
	// wg.Wait 收敛后，所有 Spawn goroutine 都应已返回。
	spawnDone := make(chan struct{})
	go func() { wg.Wait(); close(spawnDone) }()
	select {
	case <-spawnDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Spawn goroutine 未随 Close 退出")
	}

	// Close 幂等；Close 后拒绝新 Spawn。
	if err := env.c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := env.c.Spawn(context.Background(), SpawnRequest{ParentRunID: "run-parent", Input: "y"}); !errors.Is(err, errClosed) {
		t.Fatalf("Close 后 Spawn err = %v, want errClosed", err)
	}
	// 宽松的 goroutine 数检查：collab 自身不起后台 goroutine，测试起的都已被 wg 收敛。
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("疑似 goroutine 泄漏: before=%d after=%d", before, after)
	}
}
