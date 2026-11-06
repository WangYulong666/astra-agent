package collab

import (
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// openImpl 是装配桩，由并行任务替换成真实现。
// 桩返回错误而不是 nil：能力未实现时 app 层降级，平台照常启动。
func openImpl(cfg Config, st store.Store) (Coordinator, error) {
	return nil, fmt.Errorf("collab: 实现未接入（data=%s）", cfg.DataDir)
}
