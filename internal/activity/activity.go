// Package activity 把「运营视角的审计日志」翻译成「用户视角的行动账本」。
//
// 区别很实在：audit log 回答「谁在什么时候调了什么接口」，用户想问的是
// 「它做了什么、还打算做什么、卡在哪、为什么停了」。同一个 run 的素材散在
// 四个地方——事件流、审批记录、出网日志、子任务——这里聚成一条按时间排序、
// 带 status（done / running / blocked / failed）的可读时间线。
package activity

import (
	"context"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// Kind 是账本条目类型。前端据此选图标与配色。
const (
	KindPlan     = "plan"     // 用户指令 / 任务开始
	KindTool     = "tool"     // 工具调用
	KindApproval = "approval" // 人工审批（blocked 的主要来源）
	KindEgress   = "egress"   // 出网请求（allow / deny）
	KindMessage  = "message"  // agent 回复
	KindChild    = "child"    // 派生的子 agent
	KindState    = "state"    // run 状态迁移
	KindError    = "error"
)

// Status 回答「现在怎么样了」。
const (
	StatusDone    = "done"
	StatusRunning = "running"
	StatusBlocked = "blocked"
	StatusFailed  = "failed"
)

type Entry struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	Status    string    `json:"status"`
	RunID     string    `json:"runId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	// Ref 是原记录定位（toolCallId / approvalId / host / childRunId）。
	Ref string `json:"ref,omitempty"`
}

// Summary 是账本顶部的四个计数 + 一句「现在卡在哪」。
// Current 为空表示没有阻塞项。
type Summary struct {
	Done    int    `json:"done"`
	Running int    `json:"running"`
	Blocked int    `json:"blocked"`
	Failed  int    `json:"failed"`
	Current string `json:"current,omitempty"`
}

type Ledger interface {
	ForRun(ctx context.Context, runID string, limit int) ([]Entry, error)
	ForSession(ctx context.Context, sessionID string, limit int) ([]Entry, error)
	Summary(ctx context.Context, runID string) (Summary, error)
}

// 窄接口：egress.Controller 与 collab.Coordinator 天然满足，
// activity 不必反过来依赖它们的具体实现，测试也好造桩。
type EgressLog interface {
	Log(ctx context.Context, tenantID string, limit int) ([]EgressEntry, error)
}

type EgressEntry struct {
	Time      time.Time `json:"time"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	Host      string    `json:"host"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason"`
}

type ChildLister interface {
	List(ctx context.Context, parentRunID string) ([]ChildEntry, error)
}

type ChildEntry struct {
	RunID   string    `json:"runId"`
	Title   string    `json:"title"`
	Status  string    `json:"status"`
	Summary string    `json:"summary"`
	Error   string    `json:"error"`
	EndedAt time.Time `json:"endedAt"`
}

type Sources struct {
	Egress   EgressLog
	Children ChildLister
}

// RunReader 是账本需要的只读能力，store.Store 天然满足（接口隐式实现）。
// 抽出窄接口是为了让实现方能单独造桩测「事件 → 条目」的映射，不必起真实存储。
type RunReader interface {
	GetRun(ctx context.Context, id string) (store.Run, error)
	GetSession(ctx context.Context, id string) (store.Session, error)
	ListRuns(ctx context.Context, sessionID string) ([]store.Run, error)
	Events(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error)
}

// Open 装配账本。实现见 ledger.go（由子任务填充）。
func Open(st RunReader, src Sources) (Ledger, error) {
	return openImpl(st, src)
}
