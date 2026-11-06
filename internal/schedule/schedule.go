// Package schedule 让 agent 能**自己启动**，而不只是被提交后跑一次。
//
// 这是「同事感」的第一块：Grok Bot 的 routines、Muse 的「关掉 App 继续跑」，
// 本质都是一个到期就创建 run 的循环。平台原来只有 request-response，
// 用户得守着页面点。
//
// 设计取舍：
//   - 只支持「5 段 cron + 可选时区」和「webhook 触发」，不做完整工作流编排
//     （条件分支、重试策略那些等真有需求再加，先跑通最小闭环）
//   - 每个 schedule 绑定一个会话（复用会话的工作区，历史连续）
//   - 单进程内跑循环；多副本部署时靠「抢租约」避免重复触发（见 LeaseTTL）
package schedule

import (
	"context"
	"errors"
	"time"
)

// Trigger 是触发方式。
type Trigger string

const (
	// TriggerCron 按 cron 表达式。
	TriggerCron Trigger = "cron"
	// TriggerWebhook POST /v1/hooks/{token} 触发。
	TriggerWebhook Trigger = "webhook"
)

// Schedule 是一条定时/事件触发的任务。
type Schedule struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`

	Trigger Trigger `json:"trigger"`
	// Cron 只在 Trigger==cron 时有值，标准 5 段：分 时 日 月 周。
	Cron string `json:"cron,omitempty"`
	// Timezone 计算 cron 用的时区，默认 Asia/Shanghai。
	Timezone string `json:"timezone,omitempty"`
	// HookToken 只在 Trigger==webhook 时有值，出现在 URL 里，不可猜。
	HookToken string `json:"hook_token,omitempty"`

	// Prompt 每次触发提交给 agent 的指令。
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`

	// NextRunAt 下次触发时间（内存 + 落盘，重启后恢复）。
	NextRunAt time.Time `json:"next_run_at,omitempty"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
	LastRunID string    `json:"last_run_id,omitempty"`
	// LastStatus 上一次创建 run 的结果（created / skipped / failed）。
	LastStatus string `json:"last_status,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store 是 schedule 的持久化。
//
// 单独一套而不是塞进 store.Store：调度是平台能力，不是会话/run 这类运行时数据，
// 生命周期和查询模式都不一样（要按 next_run_at 范围扫）。
type Store interface {
	List(ctx context.Context, tenantID string) ([]Schedule, error)
	Get(ctx context.Context, id string) (Schedule, error)
	GetByHookToken(ctx context.Context, token string) (Schedule, error)
	Create(ctx context.Context, s Schedule) (Schedule, error)
	Update(ctx context.Context, s Schedule) error
	Delete(ctx context.Context, tenantID, id string) error
	// Due 返回 next_run_at <= now 且 enabled 的条目，按 next_run_at 升序。
	Due(ctx context.Context, now time.Time, limit int) ([]Schedule, error)
}

// Runner 是调度器真正用来「跑一次」的东西。
//
// 抽象成接口而不是直接依赖 runner.Manager：调度包不该知道 ACP、沙箱、
// 审批这些概念，它只需要「给我一个会话和一个 prompt，创建一个 run」。
type Runner interface {
	CreateRun(ctx context.Context, sessionID, prompt string) (runID string, err error)
}

// Config 装配调度器。
type Config struct {
	// Enabled 关掉时 Open 返回 Disabled，平台照常启动。
	Enabled bool
	// Tick 扫描间隔，默认 30s。
	Tick time.Duration
	// LeaseTTL 多副本抢租约的有效期，默认 90s（要大于 Tick 的 3 倍）。
	LeaseTTL time.Duration
	// MaxPerTenant 单租户最多几条，默认 50，防止误用写爆。
	MaxPerTenant int
}

// Scheduler 是调度能力的对外面。
type Scheduler interface {
	Enabled() bool
	// RunNow 立即触发一次（手动测试用），返回创建的 runID。
	RunNow(ctx context.Context, scheduleID string) (string, error)
	// Tick 手动推进一次扫描（测试用；正常由内部循环调用）。
	Tick(ctx context.Context) (triggered int, err error)
	// Start 启动后台循环。重复调用无副作用。
	Start()
	Stop()
}

var (
	ErrDisabled = errors.New("schedule: 未启用")
	ErrNotFound = errors.New("schedule: 不存在")
)
