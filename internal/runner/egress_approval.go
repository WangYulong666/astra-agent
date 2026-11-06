package runner

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/egress"
)

// RequestEgressApproval 是出网代理 Ask 规则的审批入口。
//
// 由「egress Ask 审批联动」任务替换成真实现：复用本包已有的审批注册表
// (apprs / apprStore / ResolveApproval / releaseApprovals)，把出网请求
// 登记成一条 (runID, toolCallID) 审批并阻塞等结论。放在独立新文件是为了
// 不碰 manager.go / approval.go 的已有逻辑（并行任务的文件边界）。
//
// 桩必须返回 deny：能力没接好时宁可拒绝出网，也不能静默放行。
func (m *Manager) RequestEgressApproval(ctx context.Context, req egress.ApprovalRequest) (egress.ApprovalResult, error) {
	return egress.ApprovalResult{Decision: "deny"},
		fmt.Errorf("egress 审批未接入（host=%s）", req.Host)
}
