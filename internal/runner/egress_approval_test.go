package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// egressApprovalHarness 用真实 store.OpenFile（经 newHarness）建一条「已经存在、
// 但没有 execute goroutine」的 run 记录。直接落库而不是 StartRun，是因为
// StartRun 会真的把 mock agent 跑起来，跑到终态时 releaseApprovals 会把我们
// 挂着的出网审批提前结掉，干扰断言。
func egressApprovalHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.st.CreateSession(ctx, store.Session{
		ID: "s-egress", SandboxID: "s-egress", Cwd: "/", Status: store.SessionActive,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := h.st.CreateRun(ctx, store.Run{
		ID: "run-egress", SessionID: "s-egress", Status: store.RunRunning, Input: "x",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return h, "run-egress"
}

func egressApprovalRequest(runID string, timeout time.Duration) egress.ApprovalRequest {
	return egress.ApprovalRequest{
		RunID:      runID,
		ToolCallID: "egress/api.example.com/443/abcd1234",
		Kind:       "egress",
		Title:      "允许访问 api.example.com:443",
		TenantID:   "t1",
		UserID:     "u1",
		SessionID:  "s-egress",
		Host:       "api.example.com",
		Port:       "443",
		Timeout:    timeout,
	}
}

type egressApprovalResult struct {
	r   egress.ApprovalResult
	err error
}

func startEgressApproval(m *Manager, req egress.ApprovalRequest) <-chan egressApprovalResult {
	ch := make(chan egressApprovalResult, 1)
	go func() {
		r, err := m.RequestEgressApproval(context.Background(), req)
		ch <- egressApprovalResult{r, err}
	}()
	return ch
}

func payloadMap(t *testing.T, ev store.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		t.Fatalf("解析事件 %s 失败: %v", ev.Type, err)
	}
	return m
}

func TestRequestEgressApprovalAllow(t *testing.T) {
	h, runID := egressApprovalHarness(t)
	req := egressApprovalRequest(runID, 5*time.Second)

	sub := h.hub.Subscribe(runID)
	defer sub.Close()
	ch := startEgressApproval(h.m, req)

	p := h.waitPending(t, runID)
	if p.ToolCallID != req.ToolCallID || p.Kind != "egress" {
		t.Fatalf("审批登记内容不对: %+v", p)
	}
	if err := h.m.ResolveApproval(runID, p.ToolCallID, DecisionAllow); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	got := <-ch
	if got.err != nil {
		t.Fatalf("RequestEgressApproval: %v", got.err)
	}
	if got.r.Decision != DecisionAllow {
		t.Fatalf("Decision = %q, want allow", got.r.Decision)
	}
	if got.r.ApprovalID != req.ToolCallID {
		t.Fatalf("ApprovalID = %q, want %q", got.r.ApprovalID, req.ToolCallID)
	}

	// 事件流：approval_req(pending) + approval_result(allow)。
	evs := h.eventsOf(t, runID)
	reqs := find(evs, store.EventApprovalReq)
	results := find(evs, store.EventApprovalResult)
	if len(reqs) != 1 || len(results) != 1 {
		t.Fatalf("事件数不对: req=%d result=%d", len(reqs), len(results))
	}
	rp := payloadMap(t, reqs[0])
	if rp["pending"] != true || rp["decision"] != "pending" || rp["kind"] != "egress" || rp["title"] != req.Title {
		t.Errorf("approval_req payload 不对: %+v", rp)
	}
	if rp["expiresAt"] == nil {
		t.Error("approval_req 缺 expiresAt")
	}
	sp := payloadMap(t, results[0])
	if sp["decision"] != DecisionAllow || sp["by"] != "user" || sp["toolCallId"] != req.ToolCallID {
		t.Errorf("approval_result payload 不对: %+v", sp)
	}

	// SSE：Hub.Publish 也要把两条都推出来。
	seenReq, seenResult := false, false
	for i := 0; i < 2; i++ {
		select {
		case ev := <-sub.Events():
			if ev.Type == store.EventApprovalReq {
				seenReq = true
			}
			if ev.Type == store.EventApprovalResult {
				seenResult = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("SSE 没收齐审批事件")
		}
	}
	if !seenReq || !seenResult {
		t.Errorf("SSE 事件不全: req=%v result=%v", seenReq, seenResult)
	}

	// 关键：出网挂起不改整个 run 的状态。
	if r, _ := h.st.GetRun(context.Background(), runID); r.Status != store.RunRunning {
		t.Errorf("RequestEgressApproval 不该改 run 状态，得到 %s", r.Status)
	}
}

func TestRequestEgressApprovalDeny(t *testing.T) {
	h, runID := egressApprovalHarness(t)
	req := egressApprovalRequest(runID, 5*time.Second)
	ch := startEgressApproval(h.m, req)

	p := h.waitPending(t, runID)
	if err := h.m.ResolveApproval(runID, p.ToolCallID, DecisionDeny); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	got := <-ch
	if got.err != nil {
		t.Fatalf("RequestEgressApproval: %v", got.err)
	}
	if got.r.Decision != DecisionDeny {
		t.Fatalf("Decision = %q, want deny", got.r.Decision)
	}

	evs := h.eventsOf(t, runID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 {
		t.Fatalf("approval_result 事件数 = %d, want 1", len(results))
	}
	sp := payloadMap(t, results[0])
	if sp["decision"] != DecisionDeny || sp["by"] != "user" || sp["toolCallId"] != req.ToolCallID {
		t.Errorf("approval_result payload 不对: %+v", sp)
	}
}

func TestRequestEgressApprovalTimeout(t *testing.T) {
	h, runID := egressApprovalHarness(t)
	req := egressApprovalRequest(runID, 200*time.Millisecond)

	// 没有任何人点：计时器到点应落 timeout 并按拒绝收尾。
	got, err := h.m.RequestEgressApproval(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestEgressApproval: %v", err)
	}
	if got.Decision != DecisionTimeout {
		t.Fatalf("Decision = %q, want timeout", got.Decision)
	}

	evs := h.eventsOf(t, runID)
	results := find(evs, store.EventApprovalResult)
	if len(results) != 1 {
		t.Fatalf("approval_result 事件数 = %d, want 1", len(results))
	}
	sp := payloadMap(t, results[0])
	if sp["decision"] != DecisionTimeout {
		t.Errorf("approval_result 不是 timeout: %+v", sp)
	}
	// settleApproval 应把审批表结掉，不留 pending。
	if ps := h.m.ListApprovals(runID); len(ps) != 0 {
		t.Errorf("超时后仍有 %d 条待审批", len(ps))
	}
}

// 红利：登记进同一个 m.apprs 后，releaseApprovals（run 取消/退出/重启）会自动
// 唤醒挂起的连接，无需在 egress 侧额外写取消逻辑。
func TestRequestEgressApprovalReleasedOnCancel(t *testing.T) {
	h, runID := egressApprovalHarness(t)
	req := egressApprovalRequest(runID, 5*time.Second)
	ch := startEgressApproval(h.m, req)

	h.waitPending(t, runID)
	h.m.releaseApprovals(runID, DecisionDeny)

	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("RequestEgressApproval: %v", got.err)
		}
		if got.r.Decision != DecisionDeny {
			t.Fatalf("Decision = %q, want deny（releaseApprovals 唤醒）", got.r.Decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("releaseApprovals 没有唤醒挂起的出网审批")
	}
}
