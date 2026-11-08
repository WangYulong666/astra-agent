// Package insight 把失败 run 变成下一轮可用的上下文。
//
// 明确不做「自动改平台代码」：商品化的自闭环最高只到「自动写 skill + 人工发布」，
// 自动改代码还停留在研究阶段且已出现评测作弊。这里做规则式诊断 + 回灌。
package insight

import (
	"context"
	"log"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

type Diagnosis struct {
	RunID      string    `json:"runId"`
	SessionID  string    `json:"sessionId"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	LastTool   string    `json:"lastTool,omitempty"`
	Suggestion string    `json:"suggestion"`
	ToolCount  int       `json:"toolCount"`
	Blocked    int       `json:"blocked"`
	Failed     int       `json:"failed"`
	CreatedAt  time.Time `json:"createdAt"`
	// ConsumedAt 非空表示这条诊断已被回灌进某轮 prompt，之后不再注入。
	// 指针 + omitempty：未消费的记录落盘时不带这个字段，兼容旧 insights.json。
	ConsumedAt *time.Time `json:"consumedAt,omitempty"`
}

type Summary struct {
	Done, Running, Blocked, Failed int
	Current                        string
}

// Activity 是窄接口，主控把 activity.Ledger 适配过来（insight 不 import activity）。
type Activity interface {
	Summary(ctx context.Context, runID string) (Summary, error)
}

type Store interface {
	Recent(ctx context.Context, sessionID string, limit int) ([]Diagnosis, error)
	All(ctx context.Context, limit int) ([]Diagnosis, error)
	ForRun(ctx context.Context, runID string) (Diagnosis, error)
	// Pending 只返回未被消费、且 CreatedAt 晚于 since 的诊断（CreatedAt 倒序）。
	// 回灌用它筛「最近一次成功 run 之后仍未消费」的诊断，避免历史诊断
	// 在之后每一轮被重复注入。
	Pending(ctx context.Context, sessionID string, since time.Time, limit int) ([]Diagnosis, error)
	// MarkConsumed 按 runID 把诊断标记为已消费并落盘。幂等：找不到或已消费
	// 都返回 nil，不报错。
	MarkConsumed(ctx context.Context, runID string) error
}

func Open(dataDir string) (Store, error) { return openImpl(dataDir) }

// Attach 订阅 run 终态，返回停止函数（等 goroutine 退出，不许泄漏）。
func Attach(hub *eventbus.Hub, st store.Store, act Activity, dataDir string, lg *log.Logger) func() {
	return attachImpl(hub, st, act, dataDir, lg)
}
