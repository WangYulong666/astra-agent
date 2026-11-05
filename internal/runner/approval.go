package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 审批结论。写进 approval_result 事件，前端按这三种值渲染。
const (
	DecisionAllow   = "allow"
	DecisionDeny    = "deny"
	DecisionTimeout = "timeout"
)

// approvalPollInterval 是等待方回查共享存储的间隔。
//
// 本地 channel 负责「同一进程点允许」的毫秒级响应；这个轮询负责另外两种情况：
// 点「允许」的请求落到了另一个副本，或者本进程的 map 里根本没有这条（重启后的
// 新副本接管了等待）。1s 是「人感觉不到」和「每秒一次查询」之间的折中——
// 待审批是低频事件，一次 SELECT 主键的开销远小于用户多等一秒的焦躁。
const approvalPollInterval = time.Second

// PendingApproval 是一条挂在人面前的审批请求。
type PendingApproval struct {
	RunID      string    `json:"runId"`
	ToolCallID string    `json:"toolCallId"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind"`
	CreatedAt  time.Time `json:"createdAt"`
	// ExpiresAt 到点自动按「拒绝」处理。审批不是永久的挂起状态——
	// 一个没人管的请求会把 run 和它占着的沙箱锁一起吊死。
	ExpiresAt time.Time `json:"expiresAt"`
}

// approvalWait 是注册表里的一项。ch 容量 1，保证「先到先得」不会被重复投递。
//
// 这个 map 只是快路径：真相在 store 里。进程重启后 map 是空的，但等待方
// （如果还在）靠轮询照样能收到另一个副本写下的结论。
type approvalWait struct {
	PendingApproval
	ch chan string
}

// approvalKey 用 run + toolCall 做键。
//
// 不用 toolCallId 单键：不同 run 里 agent 可能复用同一个 id（尤其重启后重放），
// 撞键的结果是把 A 会话的批准事件发给 B 的等待者——这种串台事故很难查。
func approvalKey(runID, toolCallID string) string { return runID + "/" + toolCallID }

// approvalStatusOf 把对外语义（allow/deny/timeout）映射成落库状态。
func approvalStatusOf(decision string) string {
	switch decision {
	case DecisionAllow:
		return store.ApprovalAllowed
	case DecisionTimeout:
		return store.ApprovalTimeout
	default:
		return store.ApprovalDenied
	}
}

// decisionOf 是 approvalStatusOf 的反向映射。
//
// expired 落到 deny：对 agent 来说「服务重启导致审批失效」和「没人批」一样，
// 都不能继续执行。两者靠 approval_result 的 reason 区分，不靠 decision。
func decisionOf(status string) string {
	switch status {
	case store.ApprovalAllowed:
		return DecisionAllow
	case store.ApprovalTimeout:
		return DecisionTimeout
	default:
		return DecisionDeny
	}
}

// registerApproval 把一条待审批请求挂到本地快路径上。调用方负责在结束时 delete。
func (m *Manager) registerApproval(p PendingApproval) *approvalWait {
	w := &approvalWait{PendingApproval: p, ch: make(chan string, 1)}
	m.apprMu.Lock()
	if m.apprs == nil {
		m.apprs = map[string]*approvalWait{}
	}
	m.apprs[approvalKey(p.RunID, p.ToolCallID)] = w
	m.apprMu.Unlock()
	return w
}

func (m *Manager) unregisterApproval(runID, toolCallID string) {
	m.apprMu.Lock()
	delete(m.apprs, approvalKey(runID, toolCallID))
	m.apprMu.Unlock()
}

// ResolveApproval 接收人工决策。decision 只接受 allow / deny。
//
// 先写库再投本地 channel：
//   - 写库是带 WHERE status='pending' 的条件更新，并发两次点击只有一次能赢；
//   - 本地 channel 只是「本进程有个等待者」的加速通道，投不进去不算失败——
//     等待方可能在另一个副本上，它会在 1s 内轮询到这条结论。
//
// 返回 store.ErrNotFound 表示没有这条待审批（可能已被处理、已超时、或 run 已结束）；
// 返回 store.ErrConflict 表示有这条但已经被处理过（重复点击）——
// 两者对前端的语义不同：前者应刷新状态，后者可以忽略。
func (m *Manager) ResolveApproval(runID, toolCallID, decision string) error {
	switch decision {
	case DecisionAllow, DecisionDeny:
	default:
		return fmt.Errorf("runner: 非法决策 %q（只接受 allow / deny）", decision)
	}

	if err := m.apprStore.ResolveApproval(context.Background(), runID, toolCallID,
		approvalStatusOf(decision), "user", time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: run %s 没有待审批的 %s", store.ErrNotFound, runID, toolCallID)
		}
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: %s 的审批已经处理过了", store.ErrConflict, toolCallID)
		}
		return err
	}

	m.apprMu.Lock()
	w := m.apprs[approvalKey(runID, toolCallID)]
	m.apprMu.Unlock()
	if w != nil {
		select {
		case w.ch <- decision:
		default:
			// 已经有一条结论在 channel 里，或等待方刚好走了。库里的状态是准的。
		}
	}
	return nil
}

// settleApproval 把等待方的结论落成库里的终态，并返回「库里真正生效的那一条」。
//
// 为什么还要读回来：超时计时器和用户点击可能同时到，本地可能先选了 timeout，
// 但库里是用户赢的（allowed）。事件必须和库一致，否则审计记录与实际授权相反。
func (m *Manager) settleApproval(runID, toolCallID, status, by, fallbackDecision, fallbackBy string) (string, string) {
	if err := m.apprStore.ResolveApproval(context.Background(), runID, toolCallID,
		status, by, time.Now().UTC()); err == nil {
		return decisionOf(status), by
	} else if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		if a, gerr := m.apprStore.GetApproval(context.Background(), runID, toolCallID); gerr == nil &&
			store.ApprovalResolved(a.Status) {
			resolvedBy := a.ResolvedBy
			if resolvedBy == "" {
				resolvedBy = "system"
			}
			return decisionOf(a.Status), resolvedBy
		}
	} else {
		m.log.Printf("审批 %s/%s 落终态失败: %v", runID, toolCallID, err)
	}
	// 库不可用：不能因此把 agent 永久挂住，退回本地判断。
	return fallbackDecision, fallbackBy
}

// ListApprovals 列出正在等人的审批。runID 为空表示全部。
//
// 读库而不是内存 map：多副本部署时，审批可能是在 A 副本上挂起的，
// 用户刷新页面打到了 B；只读本地 map 会得到空列表，界面于是显示「没有待审批」，
// 而 agent 还在等——这比报错更难排查。
func (m *Manager) ListApprovals(runID string) []PendingApproval {
	out := []PendingApproval{}
	rows, err := m.apprStore.ListPendingApprovals(context.Background(), runID)
	if err != nil {
		m.log.Printf("读待审批列表失败（run=%s）: %v", runID, err)
		return out
	}
	for _, a := range rows {
		out = append(out, PendingApproval{
			RunID:      a.RunID,
			ToolCallID: a.ToolCallID,
			Title:      a.Title,
			Kind:       a.Kind,
			CreatedAt:  a.CreatedAt,
			ExpiresAt:  a.ExpiresAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// releaseApprovals 把某个 run 下所有待审批一次性结掉。
// run 取消 / 失败 / 控制面退出时调用，避免等待方永久阻塞。
//
// 先落库再通知本地：库里是真相，本地通知只是让本进程的等待方立刻醒。
// 条件更新输给用户点击时（ErrConflict）什么都不用做——等待方读库会看到用户的结论。
func (m *Manager) releaseApprovals(runID, decision string) {
	status := approvalStatusOf(decision)

	ctx := context.Background()
	pending, err := m.apprStore.ListPendingApprovals(ctx, runID)
	if err != nil {
		m.log.Printf("读 run %s 的待审批失败: %v", runID, err)
	}
	for _, a := range pending {
		err := m.apprStore.ResolveApproval(ctx, a.RunID, a.ToolCallID, status, "system", time.Now().UTC())
		if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			m.log.Printf("结掉审批 %s/%s 失败: %v", a.RunID, a.ToolCallID, err)
		}
	}

	m.apprMu.Lock()
	var waits []*approvalWait
	for k, w := range m.apprs {
		if w.RunID != runID {
			continue
		}
		delete(m.apprs, k)
		waits = append(waits, w)
	}
	m.apprMu.Unlock()

	for _, w := range waits {
		select {
		case w.ch <- decision:
		default:
		}
	}
}
