package runner

import (
	"context"
	"time"

	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// RequestEgressApproval 是出网代理 Ask 规则的审批入口。
//
// 复用本包既有的人工审批设施（apprs / apprStore / ResolveApproval /
// releaseApprovals / settleApproval），把一次出网请求登记成一条
// (runID, toolCallID) 审批并阻塞等结论。出网挂起发生在一次网络请求里，
// 不应该像 awaitHuman 那样把整个 run 改成 waiting_approval（那会和正在
// 执行的 prompt 打架），所以这里**不动 run 状态**：审批表 + 事件足够让
// 前端展示「有一条出网审批在等人」。
//
// 因为登记进了同一个 m.apprs，releaseApprovals（run 取消 / 控制面退出 /
// 重启 expire）会把这条也一起按 deny 唤醒，无需在这里额外处理取消。
func (m *Manager) RequestEgressApproval(ctx context.Context, req egress.ApprovalRequest) (egress.ApprovalResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = m.cfg.ApprovalTimeout
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
	}

	now := time.Now().UTC()
	pending := PendingApproval{
		RunID:      req.RunID,
		ToolCallID: req.ToolCallID,
		Title:      req.Title,
		Kind:       "egress",
		CreatedAt:  now,
		ExpiresAt:  now.Add(timeout),
	}

	// storeCtx 去掉请求 ctx 的取消/超时：审批要落库、事件要落库并推给 SSE，
	// 不能因为「沙箱断开导致 ctx 被取消」就丢掉这些记录。
	storeCtx := context.WithoutCancel(ctx)

	// 库是跨进程的唯一真相；写失败只留痕（本副本仍可处理），不直接拒绝——
	// 与 awaitHuman 一致。
	if err := m.apprStore.CreateApproval(storeCtx, store.Approval{
		RunID:      pending.RunID,
		ToolCallID: pending.ToolCallID,
		Title:      pending.Title,
		Kind:       pending.Kind,
		Status:     store.ApprovalPending,
		CreatedAt:  pending.CreatedAt,
		ExpiresAt:  pending.ExpiresAt,
	}); err != nil {
		m.log.Printf("出网审批 %s/%s 落库失败（只有本进程能处理本次审批）: %v",
			req.RunID, req.ToolCallID, err)
	}

	w := m.registerApproval(pending)
	defer m.unregisterApproval(req.RunID, req.ToolCallID)

	// 事件不走 sink（它绑定 run 执行生命周期，出网挂起时未必有活动 sink）。
	// 直接 Append + Publish：Append 分配 seq 并落库，Publish 推给 SSE 订阅，
	// 两者合起来就是前端看到的那条审批时间线。
	m.emitEgressEvent(storeCtx, req.RunID, store.EventApprovalReq, map[string]any{
		"toolCallId": req.ToolCallID,
		"title":      req.Title,
		"kind":       "egress",
		"host":       req.Host,
		"port":       req.Port,
		"decision":   "pending",
		"allowed":    false,
		"pending":    true,
		"expiresAt":  pending.ExpiresAt,
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()

	var decision, by string
wait:
	for {
		select {
		case decision = <-w.ch:
			by = "user"
			break wait
		case <-ticker.C:
			// 跨副本 / 跨进程：另一个副本改了库，本进程的 channel 永远不会响。
			a, err := m.apprStore.GetApproval(context.Background(), req.RunID, req.ToolCallID)
			if err != nil || !store.ApprovalResolved(a.Status) {
				continue
			}
			decision = decisionOf(a.Status)
			by = a.ResolvedBy
			if by == "" {
				by = "system"
			}
			break wait
		case <-timer.C:
			decision, by = DecisionTimeout, "system"
			break wait
		case <-ctx.Done():
			// 沙箱客户端断开 / 代理 Close：按拒绝收尾，别把连接吊死。
			decision, by = DecisionDeny, "system"
			break wait
		case <-m.baseCtx.Done():
			decision, by = DecisionDeny, "system"
			break wait
		}
	}

	// 本地决定可能输给库里的另一个写入方（用户点击 vs 超时 / 取消）。
	decision, by = m.settleApproval(req.RunID, req.ToolCallID,
		approvalStatusOf(decision), by, decision, by)

	m.log.Printf("出网审批 %s/%s → %s（%s）", req.RunID, req.ToolCallID, decision, by)
	m.emitEgressEvent(storeCtx, req.RunID, store.EventApprovalResult, map[string]any{
		"toolCallId": req.ToolCallID,
		"decision":   decision,
		"by":         by,
	})

	return egress.ApprovalResult{
		Decision:   decision,
		ApprovalID: req.ToolCallID,
	}, nil
}

// emitEgressEvent 把一条审批事件落库并扇出到 SSE。落库失败只留痕：事件丢了
// 不该让已经有人点过「允许」的出网请求改成拒绝。
func (m *Manager) emitEgressEvent(ctx context.Context, runID, typ string, payload any) {
	ev, err := m.cfg.Store.Append(ctx, runID, store.NewEvent{Type: typ, Payload: payload})
	if err != nil {
		m.log.Printf("出网审批事件 %s/%s 落库失败: %v", runID, typ, err)
		return
	}
	if m.cfg.Hub != nil {
		m.cfg.Hub.Publish(ev)
	}
}
