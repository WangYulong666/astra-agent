package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 这三条用例覆盖审批从「进程内」搬到「共享存储」后必须成立的性质：
//
//  1. 并发点击只有一次赢——审批是授权动作，重复生效等于越权；
//  2. 另一个进程写库也能唤醒本进程的等待方——多副本/重启后的核心价值；
//  3. 重启恢复把悬挂审批标成 expired 并留下事件——界面不能永远转圈。

func TestResolveApprovalConcurrentOnlyOneWins(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy = Ask() })
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "把结果写进 notes.md")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	p := h.waitPending(t, run.ID)

	const clickers = 2
	start := make(chan struct{})
	errs := make(chan error, clickers)
	var wg sync.WaitGroup
	for i := 0; i < clickers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- h.m.ResolveApproval(run.ID, p.ToolCallID, DecisionAllow)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var ok, conflict int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrConflict):
			conflict++
		default:
			t.Fatalf("并发点击返回了意外错误: %v", err)
		}
	}
	if ok != 1 || conflict != clickers-1 {
		t.Fatalf("成功 %d 次、冲突 %d 次，期望 1 / %d", ok, conflict, clickers-1)
	}
	h.waitTerminal(t, run.ID)
}

// 绕过 Manager 的本地 map，直接改共享存储，模拟「另一个副本处理了这条审批」。
// 等待方必须在一个轮询周期内看到结论并继续，否则多副本部署根本不能用。
func TestApprovalResolvedByAnotherProcessWakesWaiter(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy = Ask() })
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "把结果写进 notes.md")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	p := h.waitPending(t, run.ID)

	// 关键：不调用 h.m.ResolveApproval（那会往本地 channel 投递），
	// 只走 store，等价于另一个副本 / 另一个进程写库。
	if err := h.st.ResolveApproval(ctx, run.ID, p.ToolCallID,
		store.ApprovalAllowed, "other-replica", time.Now().UTC()); err != nil {
		t.Fatalf("模拟另一个进程写库失败: %v", err)
	}

	started := time.Now()
	h.waitTerminal(t, run.ID)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("等待方 %v 才看到结论，轮询没有生效", elapsed)
	}

	evs := h.eventsOf(t, run.ID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 {
		t.Fatalf("approval_result 数 = %d，应为 1", len(results))
	}
	if !strings.Contains(string(results[0].Payload), `"decision":"allow"`) ||
		!strings.Contains(string(results[0].Payload), `"by":"other-replica"`) {
		t.Fatalf("结论没有采用库里的那一条: %s", results[0].Payload)
	}
	if txt := text(evs, store.EventMessageDelta); !strings.Contains(txt, "notes.md") {
		t.Fatalf("agent 没有继续产出，message_delta=%q", txt)
	}
}

// 重启恢复：pending 审批必须变 expired，run 进终态，且留下 approval_result。
// 没有这条事件，前端只看到 run 失败，不知道那张「等你决定」的卡片为什么消失。
func TestRecoverInterruptedExpiresPendingApproval(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	sess := store.Session{
		ID: "sess-recover", SandboxID: "sess-recover",
		Status: store.SessionActive, CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
	}
	if err := h.st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run := store.Run{
		ID: "run-recover", SessionID: sess.ID, Status: store.RunWaitingApproval,
		Input: "rm -rf build", CreatedAt: time.Now().UTC(),
	}
	if err := h.st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := h.st.CreateApproval(ctx, store.Approval{
		RunID: run.ID, ToolCallID: "tc-recover", Title: "rm -rf build", Kind: "execute",
		Status: store.ApprovalPending, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	n, err := h.m.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 1 {
		t.Fatalf("归档了 %d 个 run，应为 1", n)
	}

	a, err := h.st.GetApproval(ctx, run.ID, "tc-recover")
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if a.Status != store.ApprovalExpired {
		t.Fatalf("审批状态 = %s，应为 %s", a.Status, store.ApprovalExpired)
	}
	if a.ResolvedBy != "system" || a.ResolvedAt.IsZero() {
		t.Fatalf("失效审批缺少归档信息: %+v", a)
	}
	if ps, err := h.st.ListPendingApprovals(ctx, run.ID); err != nil || len(ps) != 0 {
		t.Fatalf("恢复后仍有待审批: %+v err=%v", ps, err)
	}

	got, err := h.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != store.RunFailed {
		t.Fatalf("run 状态 = %s，应为 %s", got.Status, store.RunFailed)
	}
	if got.EndedAt.IsZero() || got.Error == "" {
		t.Fatalf("终态信息不完整: %+v", got)
	}

	evs := h.eventsOf(t, run.ID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 {
		t.Fatalf("approval_result 数 = %d，应为 1", len(results))
	}
	payload := string(results[0].Payload)
	if !strings.Contains(payload, `"reason":"expired"`) || !strings.Contains(payload, `"toolCallId":"tc-recover"`) {
		t.Fatalf("approval_result 没说明失效原因: %s", payload)
	}
	if len(find(evs, store.EventState)) == 0 {
		t.Fatal("缺 run_state 终态事件")
	}
}
