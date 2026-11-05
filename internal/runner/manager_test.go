package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

var (
	proxyBin string
	mockBin  string
	binErr   error
)

// TestMain 现编 acp-proxy 与 mock agent。
//
// 用真二进制而不是打桩：这条链路的坑几乎都在进程/连接边界上
// （端口分配、WS 分帧、进程退出），内嵌 fake 恰好绕过这些。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "astra-runner-bin")
	if err != nil {
		binErr = err
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	proxyBin = filepath.Join(dir, "acp-proxy")
	mockBin = filepath.Join(dir, "acp-mock-agent")
	for out, pkg := range map[string]string{
		proxyBin: "../../cmd/acp-proxy",
		mockBin:  "../../cmd/acp-mock-agent",
	} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			binErr = err
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

type harness struct {
	m    *Manager
	st   *store.File
	hub  *eventbus.Hub
	root string
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	if binErr != nil {
		t.Fatalf("准备二进制失败: %v", binErr)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("没有 go 命令，跳过集成测试")
	}

	root := t.TempDir()
	if os.Getenv("ASTRA_KEEP") != "" {
		root, _ = os.MkdirTemp("", "astra-keep")
		t.Logf("保留测试目录 %s", root)
	}
	st, err := store.OpenFile(filepath.Join(root, "data"), store.Options{})
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	p, err := sandbox.New("local", sandbox.Options{
		Root:     filepath.Join(root, "sandboxes"),
		ProxyBin: proxyBin,
	})
	if err != nil {
		t.Fatalf("建 provider: %v", err)
	}

	hub := eventbus.New()
	cfg := Config{
		Store:    st,
		Provider: p,
		Hub:      hub,
		// ASTRA_MOCK_STATE 让 mock agent 把会话 id 落盘：跨进程 resume 才可测
		// （dsh 靠的是 /var/lib/dsh，是同一回事）。
		Agent: sandbox.AgentSpec{
			Command: []string{mockBin},
			Env:     map[string]string{"ASTRA_MOCK_STATE": filepath.Join(root, "mock-state")},
		},
		Policy:         WorkspaceWrite(),
		CoalesceWindow: 20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	return &harness{m: m, st: st, hub: hub, root: root}
}

// collect 订阅并收集事件，直到 run 进入终态或超时。
func (h *harness) collect(t *testing.T, runID string) []store.Event {
	t.Helper()
	sub := h.hub.Subscribe(runID)
	defer sub.Close()

	deadline := time.After(30 * time.Second)
	var evs []store.Event
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("订阅被关闭（消费太慢）")
			}
			evs = append(evs, ev)
			if ev.Type == store.EventState {
				var p struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(ev.Payload, &p)
				if store.IsTerminal(p.Status) {
					return evs
				}
			}
		case <-deadline:
			t.Fatalf("等终态超时，已收到 %d 条事件", len(evs))
		}
	}
}

// waitTerminal 轮询 store 直到 run 进入终态。
// 用轮询而不是订阅：短任务可能在订阅前就跑完，靠 hub 会漏掉终态事件。
func (h *harness) waitTerminal(t *testing.T, runID string) store.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, err := h.st.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("读 run: %v", err)
		}
		if store.IsTerminal(r.Status) {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等 run %s 终态超时", runID)
	return store.Run{}
}

func (h *harness) eventsOf(t *testing.T, runID string) []store.Event {
	t.Helper()
	evs, err := h.st.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatalf("读事件: %v", err)
	}
	return evs
}

func text(evs []store.Event, typ string) string {
	var sb strings.Builder
	for _, e := range evs {
		if e.Type != typ {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func find(evs []store.Event, typ string) []store.Event {
	var out []store.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestRunStreamsCoalescedEvents(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.AgentSessionID == "" {
		t.Fatal("agentSessionId 为空")
	}

	run, err := h.m.StartRun(ctx, sess.ID, "把结果写进 notes.md")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	evs := h.collect(t, run.ID)

	// 1) seq 必须从 1 连续递增——客户端靠它续传，断一个数就漏事件
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq=%d，应为 %d", i, ev.Seq, i+1)
		}
	}

	// 2) 终态
	last := evs[len(evs)-1]
	var st struct {
		Status     string `json:"status"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(last.Payload, &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != store.RunCompleted || st.StopReason != "end_turn" {
		t.Fatalf("终态 = %s/%s，期望 completed/end_turn", st.Status, st.StopReason)
	}

	// 3) 文本被合并过：mock 每块 4 字，回复共 11 字 → 合并后条数应远少于 11
	msgs := find(evs, store.EventMessageDelta)
	if len(msgs) == 0 {
		t.Fatal("没有任何 message_delta")
	}
	if got := text(evs, store.EventMessageDelta); got != "已处理：把结果写进 notes.md" {
		t.Fatalf("合并后文本 = %q", got)
	}
	if len(msgs) >= 11 {
		t.Errorf("增量没被合并：%d 条（逐 token 会造成事件表膨胀）", len(msgs))
	}

	// 4) 思考流、工具调用、审批留痕都要在
	if len(find(evs, store.EventThoughtDelta)) == 0 {
		t.Error("缺 thought_delta")
	}
	if len(find(evs, store.EventToolStart)) == 0 {
		t.Error("缺 tool_call_start")
	}
	if len(find(evs, store.EventToolUpdate)) == 0 {
		t.Error("缺 tool_call_update")
	}
	ap := find(evs, store.EventApprovalReq)
	if len(ap) != 1 {
		t.Fatalf("应有 1 条 approval_req，实际 %d", len(ap))
	}
	var apPayload struct {
		Policy   string `json:"policy"`
		Decision string `json:"decision"`
		Allowed  bool   `json:"allowed"`
	}
	if err := json.Unmarshal(ap[0].Payload, &apPayload); err != nil {
		t.Fatal(err)
	}
	if !apPayload.Allowed || apPayload.Decision != "allow_once" || apPayload.Policy != "workspace-write" {
		t.Errorf("审批留痕不符: %+v", apPayload)
	}

	// 5) run 元数据落库
	saved, err := h.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != store.RunCompleted || saved.EndedAt.IsZero() {
		t.Errorf("run 未正确归档: %+v", saved)
	}

	// 6) 断线续传：after_seq 能取到剩余事件
	tail, err := h.st.Events(ctx, run.ID, int64(len(evs)-2), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[1].Seq != int64(len(evs)) {
		t.Errorf("续传结果不符: %d 条", len(tail))
	}
}

func TestPermissionDenied(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy = DenyAll() })
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "写点什么")
	if err != nil {
		t.Fatal(err)
	}
	saved := h.waitTerminal(t, run.ID)
	evs := h.eventsOf(t, run.ID)

	ap := find(evs, store.EventApprovalReq)
	if len(ap) != 1 {
		t.Fatalf("应有 1 条 approval_req，实际 %d", len(ap))
	}
	var p struct {
		Allowed  bool   `json:"allowed"`
		Decision string `json:"decision"`
	}
	_ = json.Unmarshal(ap[0].Payload, &p)
	if p.Allowed || p.Decision != "reject_once" {
		t.Errorf("拒绝策略下审批应为 reject_once，实际 %+v", p)
	}

	if saved.StopReason != "refusal" {
		t.Errorf("stopReason = %q，期望 refusal（拒绝后 agent 会放弃）", saved.StopReason)
	}
	if saved.Status != store.RunCompleted {
		t.Errorf("拒稿也属于正常结束，status = %q", saved.Status)
	}
}

func TestCancelMarksRunCancelled(t *testing.T) {
	// 把 mock agent 的每块间隔拉长：默认 12ms 时一个 120 字的 prompt 只要 180ms，
	// 「等首条事件 → 发取消」的往返经常跑在 agent 已经结束之后，
	// 于是断言到 completed。之前这条用例是偶发红的——红在用例本身，不是代码。
	h := newHarness(t, func(c *Config) {
		if c.Agent.Env == nil {
			c.Agent.Env = map[string]string{}
		}
		c.Agent.Env["ASTRA_MOCK_DELAY_MS"] = "200"
	})
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, strings.Repeat("长任务", 40))
	if err != nil {
		t.Fatal(err)
	}

	// 等第一块增量出现，确保 prompt 真的在跑
	sub := h.hub.Subscribe(run.ID)
	defer sub.Close()
	select {
	case <-sub.Events():
	case <-time.After(10 * time.Second):
		t.Fatal("等首条事件超时")
	}

	if err := h.m.Cancel(ctx, run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-sub.Events():
		case <-deadline:
			t.Fatal("取消后未在 20s 内结束")
		default:
		}
		saved, err := h.st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Status == store.RunCancelled {
			if saved.StopReason != "cancelled" {
				t.Errorf("stopReason = %q，期望 cancelled", saved.StopReason)
			}
			return
		}
		if store.IsTerminal(saved.Status) {
			t.Fatalf("期望 cancelled，实际 %s", saved.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 同一会话并发两个 prompt：必须串行，否则 agent 会以「已有进行中的 prompt」拒绝。
func TestConcurrentRunsAreSerialized(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}

	r1, err := h.m.StartRun(ctx, sess.ID, "第一个")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.m.StartRun(ctx, sess.ID, "第二个")
	if err != nil {
		t.Fatal(err)
	}

	h.waitTerminal(t, r1.ID)
	h.waitTerminal(t, r2.ID)
	ev1 := h.eventsOf(t, r1.ID)
	ev2 := h.eventsOf(t, r2.ID)
	if got := text(ev2, store.EventMessageDelta); got != "已处理：第二个" {
		t.Errorf("第二个 run 输出异常: %q（可能两个 prompt 撞在一起了）", got)
	}
	if len(ev1) == 0 {
		t.Error("第一个 run 没有事件")
	}
	for _, r := range []string{r1.ID, r2.ID} {
		saved, _ := h.st.GetRun(ctx, r)
		if saved.Status != store.RunCompleted {
			t.Errorf("run %s 状态 = %s", r, saved.Status)
		}
	}
}

// 沙箱被杀掉（模拟机器重启/空闲回收）后，下一次 prompt 应能自愈：
// 重新拉起沙箱 + resume 旧会话，而不是报错让用户重开会话。
func TestRecoversAfterAgentRestart(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run1, err := h.m.StartRun(ctx, sess.ID, "第一次")
	if err != nil {
		t.Fatal(err)
	}
	h.collect(t, run1.ID)

	// 暂停沙箱：进程被回收、目录与 home 保留。这是真实场景（空闲回收/机器重启），
	// 与「连数据一起销毁」不同——后者会话状态真的没了，resume 必然失败。
	sb, err := h.m.cfg.Provider.Acquire(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := sb.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// 让 runner 手里的连接失效
	h.m.mu.Lock()
	if b := h.m.binds[sess.ID]; b != nil {
		b.mu.Lock()
		if b.client != nil {
			_ = b.client.Conn().Close()
			b.client = nil
		}
		b.mu.Unlock()
	}
	h.m.mu.Unlock()

	run2, err := h.m.StartRun(ctx, sess.ID, "第二次")
	if err != nil {
		t.Fatal(err)
	}
	saved := h.waitTerminal(t, run2.ID)
	evs := h.eventsOf(t, run2.ID)
	if got := text(evs, store.EventMessageDelta); got != "已处理：第二次" {
		t.Fatalf("重启后输出异常: %q", got)
	}
	if saved.Status != store.RunCompleted {
		t.Fatalf("重启后 run 状态 = %s", saved.Status)
	}
}
