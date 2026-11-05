package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 这三个用例验证的是同一件事的不同出口：**审批真的会阻塞 agent**。
// 如果哪天有人把 awaitHuman 改成「先回允许再记日志」，用例 2（拒绝）会立刻红——
// 因为 agent 会拿到 allow，然后继续往下走。

// waitPending 等到审批注册表里出现某个 run 的请求。
func (h *harness) waitPending(t *testing.T, runID string) PendingApproval {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ps := h.m.ListApprovals(runID); len(ps) > 0 {
			return ps[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等审批出现超时（run=%s）", runID)
	return PendingApproval{}
}

func TestApprovalAllowLetsAgentContinue(t *testing.T) {
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
	if p.Kind != "edit" || p.Title == "" {
		t.Fatalf("审批请求内容不对: %+v", p)
	}
	// 挂起期间 run 必须是 waiting_approval，否则前端不知道自己在等什么。
	if got, _ := h.st.GetRun(ctx, run.ID); got.Status != store.RunWaitingApproval {
		t.Fatalf("run 状态 = %s，应为 %s", got.Status, store.RunWaitingApproval)
	}

	if err := h.m.ResolveApproval(run.ID, p.ToolCallID, DecisionAllow); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	h.waitTerminal(t, run.ID)

	evs := h.eventsOf(t, run.ID)
	reqs := find(evs, store.EventApprovalReq)
	if len(reqs) != 1 {
		t.Fatalf("approval_req 事件数 = %d，应为 1", len(reqs))
	}
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 {
		t.Fatalf("approval_result 事件数 = %d，应为 1", len(results))
	}
	if !strings.Contains(string(results[0].Payload), `"decision":"allow"`) {
		t.Fatalf("审批结论不是 allow: %s", results[0].Payload)
	}
	// 允许之后 agent 应该继续跑完，而不是被我们的超时兜底打断。
	if txt := text(evs, store.EventMessageDelta); !strings.Contains(txt, "notes.md") {
		t.Fatalf("agent 没有继续产出，message_delta=%q", txt)
	}
}

func TestApprovalDenyStopsAgent(t *testing.T) {
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
	if err := h.m.ResolveApproval(run.ID, p.ToolCallID, DecisionDeny); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	h.waitTerminal(t, run.ID)

	evs := h.eventsOf(t, run.ID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 || !strings.Contains(string(results[0].Payload), `"decision":"deny"`) {
		t.Fatalf("审批结论不是 deny: %+v", results)
	}
	// 拒绝后 agent 必须收到「没被允许」：mock agent 在拒绝分支把工具调用标成 failed。
	var rejected bool
	for _, ev := range find(evs, store.EventToolUpdate) {
		if strings.Contains(string(ev.Payload), "权限被拒绝") {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("拒绝后没有产生 tool_call_update(failed)，说明 agent 那边根本没收到拒绝")
	}
	if got, _ := h.st.GetRun(ctx, run.ID); got.StopReason != "refusal" {
		t.Fatalf("拒绝后 stopReason = %q，应为 refusal", got.StopReason)
	}
}

func TestApprovalTimeoutFallsBackToDeny(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Policy = Ask()
		c.ApprovalTimeout = 300 * time.Millisecond
	})
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "把结果写进 notes.md")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// 什么都不点，等它自己超时。没人管的审批不能把 run 吊死。
	h.waitTerminal(t, run.ID)

	evs := h.eventsOf(t, run.ID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 || !strings.Contains(string(results[0].Payload), `"decision":"timeout"`) {
		t.Fatalf("超时应落 decision=timeout: %+v", results)
	}
}

// 取消时要把挂着的审批一起结掉，否则等待方永远阻塞在 select 上，
// 控制面退出会卡在 wg.Wait。
func TestCancelReleasesPendingApproval(t *testing.T) {
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
	h.waitPending(t, run.ID)

	if err := h.m.Cancel(ctx, run.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// 在主 goroutine 里轮询：从子 goroutine 调 t.Fatalf 会让测试提前返回，
	// TempDir 与 Close 抢先清场，看起来像「取消把 run 弄坏了」。
	r := h.waitTerminal(t, run.ID)
	// 两种都算对：agent 先看到「审批被拒」就返回 refusal，或先看到 cancel 就返回 cancelled。
	// 不允许的是 failed / 一直挂着——前者说明取消被当成了错误，后者说明审批没释放。
	if r.Status != store.RunCancelled && r.Status != store.RunCompleted {
		t.Fatalf("取消后状态 = %s（stopReason=%s error=%s）", r.Status, r.StopReason, r.Error)
	}
	if ps := h.m.ListApprovals(run.ID); len(ps) != 0 {
		t.Fatalf("取消后仍有 %d 条待审批", len(ps))
	}
}
