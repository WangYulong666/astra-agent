package egress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// defaultAskTimeout 与 Config.AskTimeout 的 0 值语义一致（egress.go 冻结，
// 默认值在这里兜底，不往 Config 里加字段）。
const defaultAskTimeout = 5 * time.Minute

// newEgressToolCallID 合成一条出网审批的 toolCallID。
//
// 前缀 egress/ 加 8 位随机 hex 是为了避开 ACP 的 toolCallId 命名空间：
// approval.go:49-53 警告过「不同 run 复用同一个 toolCallId 会把 A 的批准
// 发给 B」，而审批键是 (runID, toolCallID)。随机后缀保证同一主机同一时刻
// 的并发请求也不会撞键。
func newEgressToolCallID(host, port string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 基本不会失败；真失败就用时间戳兜底，仍是 8 位 hex。
		n := uint32(time.Now().UnixNano())
		b = [4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return "egress/" + host + "/" + port + "/" + hex.EncodeToString(b[:])
}

func (p *proxy) effectiveAskTimeout() time.Duration {
	if p.askTimeout > 0 {
		return p.askTimeout
	}
	return defaultAskTimeout
}

// askContext 把三种取消信号合进一个 ctx，交给 Approver.Request：
//   - r.Context().Done()：沙箱客户端断开，别把 handler 吊在一个没人等的连接上；
//   - p.closedCh：代理 Close，挂起的 handler 必须立刻按 Deny 收尾；
//   - timeout：审批等待上限，超时也按 Deny 收尾。
//
// 这样 Approver 只要尊重 ctx，handler 就不会卡死。
func (p *proxy) askContext(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	stop := make(chan struct{})
	go func() {
		select {
		case <-p.closedCh:
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

// handleAsk 处理 dec==Ask 的一次请求，返回 (*LogEntry, 最终决策, reason)。
//
//   - 返回非 nil 的 *LogEntry 表示本函数已在「挂起前」落了审计、并在结论后
//     回填了 Decision / Reason / ApprovalID，调用方不要再 record；
//   - 返回 nil 表示前置条件不满足、直接降级 Deny，由调用方按常规 record。
//
// 前置条件三缺一即降级（绝不静默放行）：没有审批通道 / 认不出归属 / 没有 run。
// 顺序上先查 Approver，保证「审批根本没接」时原因稳定为 ask-no-approver。
func (p *proxy) handleAsk(r *http.Request, o owner, host, port, reason string, started time.Time) (*LogEntry, Decision, string) {
	if p.approver == nil {
		return nil, Deny, "ask-no-approver"
	}
	if !o.known() || o.RunID == "" {
		return nil, Deny, "ask-no-run"
	}

	toolCallID := newEgressToolCallID(host, port)
	timeout := p.effectiveAskTimeout()

	// 挂起前先落一条审计（决策为 Ask、带上 ApprovalID）：等待期间运营就能
	// 看到「有一条出网审批挂着」，拿到结论后再回填最终字段。
	entry := p.record(o, host, port, Ask, reason, started)
	p.ring.update(entry, func(e *LogEntry) { e.ApprovalID = toolCallID })

	askCtx, done := p.askContext(r, timeout)
	defer done()

	result, err := p.approver.Request(askCtx, ApprovalRequest{
		RunID:      o.RunID,
		ToolCallID: toolCallID,
		Kind:       "egress",
		Title:      fmt.Sprintf("允许访问 %s:%s", host, port),
		TenantID:   o.TenantID,
		UserID:     o.UserID,
		SessionID:  o.SessionID,
		Host:       host,
		Port:       port,
		Timeout:    timeout,
	})

	finalDec := Deny
	finalReason := "ask-denied"
	switch {
	case err != nil:
		finalReason = "ask-error"
	case result.Decision == "allow":
		finalDec = Allow
		finalReason = "ask-approved"
	case result.Decision == "timeout":
		finalReason = "ask-timeout"
	}
	approvalID := result.ApprovalID
	if approvalID == "" {
		approvalID = toolCallID
	}
	p.ring.update(entry, func(e *LogEntry) {
		e.Decision = finalDec
		e.Reason = finalReason
		e.ApprovalID = approvalID
	})
	return entry, finalDec, finalReason
}
