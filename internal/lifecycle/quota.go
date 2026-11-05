package lifecycle

// 配额：创建会话前的准入控制。
//
// 与 Reaper 分开的理由（见 policy.go）：配额是同步、确定性、必须当场给答案的；
// 回收是异步兜底，可能延迟一拍。把两者混成一个「超限就删一个旧的」逻辑，
// 等于让新用户的操作去牺牲老用户的数据，这在多租户下是事故而不是策略。

import (
	"context"
	"errors"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// Check 判断该租户还能不能再起一个沙箱。
//
// 返回 nil 放行；超限返回包装过的 ErrQuotaExceeded（保留 errors.Is 语义，
// 上层映射 429 时不必解析字符串）。
func (q *Quota) Check(ctx context.Context, tenantID string) error {
	if q.MaxPerTenant <= 0 {
		// 0 = 不限。这里必须最先返回：否则「未配置配额」会变成「一个都不许建」，
		// 那是把开关装反了。
		return nil
	}
	if q.Store == nil {
		return errors.New("lifecycle: 配额缺少 Store")
	}

	// 用 ListSessions 再过滤，而不是给 Store 加 CountSessions(tenant)：
	// Store 是实现方要共同维护的公共契约，为一次计数就扩接口，会让所有实现
	// （file / sqlite / postgres）和所有假实现一起改。会话量在单租户维度是几千级，
	// 全量列举的代价远低于接口分裂的代价。
	sessions, err := q.Store.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("统计租户沙箱数失败: %w", err)
	}

	used := 0
	for _, s := range sessions {
		if s.TenantID != tenantID {
			continue
		}
		// 只有 active 会话占坑：deleted / idle 的沙箱已经被（或即将被）回收，
		// 把它们算进配额会让用户「删了会话也建不了新的」。
		if s.Status != store.SessionActive {
			continue
		}
		// SandboxID 为空表示这条会话没有对应沙箱（历史数据 / 建会话失败的回滚残留），
		// 它不消耗并发额度。
		if s.SandboxID == "" {
			continue
		}
		used++
	}

	if used >= q.MaxPerTenant {
		// 文案带「当前 N / 上限 M」：前端可以直接展示，不用自己再查一次；
		// 用户看到数字才知道是「清一个」还是「申请提额」。
		return fmt.Errorf("%w（当前 %d / 上限 %d）", ErrQuotaExceeded, used, q.MaxPerTenant)
	}
	return nil
}
