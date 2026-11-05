// Package trace 把平台内部的 run 生命周期映射成可观测 trace（Langfuse）。
//
// 与 internal/obs 的区别：obs 是 Prometheus 指标（数量/延迟/错误率），
// trace 是一次任务的完整因果链（prompt、工具入参、审批决策）。
//
// 为什么不直接埋 LLM SDK：agent 在沙箱里由 dsh 调用模型，平台看不到那个调用。
// 平台能看到的是 ACP 事件（turn 起止、tool call、审批）。所以这里以
// 「run = trace，turn = generation，tool call = span」的粒度映射，
// 既能还原一次任务的全貌，又不侵入 agent 内部。
package trace

import (
	"context"
	"errors"
	"time"
)

// Config 装配 Langfuse。
type Config struct {
	Endpoint  string // LANGFUSE_HOST，如 http://127.0.0.1:3000；空 = 未启用
	PublicKey string
	SecretKey string
	// SampleRate 0~1，1 表示全采样。默认 1。
	SampleRate float64
}

// Run 是一次任务（trace）的元信息。
type Run struct {
	RunID     string
	SessionID string
	TenantID  string
	UserID    string
	Agent     string
	Provider  string
	Input     string // 用户指令
	StartedAt time.Time
}

// Turn 是一轮对话（generation）。
type Turn struct {
	RunID     string
	TurnIndex int
	Input     string
	Output    string
	StartedAt time.Time
	EndedAt   time.Time
}

// ToolCall 是一次工具调用（span）。
type ToolCall struct {
	RunID      string
	ToolCallID string
	Name       string
	Input      string
	Output     string
	Decision   string // allow / deny
	StartedAt  time.Time
	EndedAt    time.Time
}

// Filter 是运营页查询 trace 的条件。
type Filter struct {
	RunID string
	Limit int
}

// Summary 是 trace 列表项（不含完整 payload，列表页不需要）。
type Summary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Status    string    `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	URL       string    `json:"url,omitempty"`
	Input     string    `json:"input,omitempty"`
}

// Tracer 是观测出口。未配置时返回 Disabled，调用点无需判空。
type Tracer interface {
	Enabled() bool
	RunStarted(ctx context.Context, r Run)
	RunEnded(ctx context.Context, runID, status, stopReason string)
	TurnRecorded(ctx context.Context, t Turn)
	ToolCallRecorded(ctx context.Context, tc ToolCall)
	// QueryTraces 供运营页/CLI 反查（服务端持 secret key，浏览器拿不到）。
	QueryTraces(ctx context.Context, f Filter) ([]Summary, error)
	// Flush 在进程退出前把缓冲刷出去（Langfuse 是批量上报）。
	Flush(ctx context.Context) error
	Close() error
}

var ErrDisabled = errors.New("trace: 未启用（未配置 Langfuse）")

// Open 按配置返回实现。
func Open(cfg Config) (Tracer, error) {
	if cfg.Endpoint == "" {
		return Disabled(), nil
	}
	return openLangfuse(cfg)
}

type disabled struct{}

func Disabled() Tracer { return disabled{} }

func (disabled) Enabled() bool                                    { return false }
func (disabled) RunStarted(context.Context, Run)                  {}
func (disabled) RunEnded(context.Context, string, string, string) {}
func (disabled) TurnRecorded(context.Context, Turn)               {}
func (disabled) ToolCallRecorded(context.Context, ToolCall)       {}
func (disabled) QueryTraces(context.Context, Filter) ([]Summary, error) {
	return nil, ErrDisabled
}
func (disabled) Flush(context.Context) error { return nil }
func (disabled) Close() error                { return nil }
