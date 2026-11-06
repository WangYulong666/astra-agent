// Package store 是会话与事件流的持久化层。
//
// 语义照架构方案里的 PG 表设计来（runs / run_events），但 M1 单进程阶段
// 用一个无外部依赖的本地实现：每个 run 一个 JSONL 文件，追加写 + fsync。
//
// 为什么不用 SQLite：M1 只需要「按 seq 追加、按 after_seq 回放」两件事，
// 换不来 SQL 能力的收益，却要背上 cgo 或一个几十 MB 的纯 Go 驱动。
// 为什么仍然定成接口：PG 实现在多副本阶段必须补上（跨进程分配 seq 要用
// `UPDATE runs SET next_seq = next_seq + 1 RETURNING`），届时上层不应该改。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 会话与 run 的状态取值。与架构文档的状态机一致。
const (
	SessionActive  = "active"
	SessionIdle    = "idle"
	SessionDeleted = "deleted"

	RunQueued          = "queued"
	RunRunning         = "running"
	RunWaitingApproval = "waiting_approval"
	RunCompleted       = "completed"
	RunFailed          = "failed"
	RunCancelled       = "cancelled"

	// 审批状态。pending 是唯一会被等待的状态，其余四个都是终态。
	// 与 runner 的 decision（allow/deny/timeout）刻意分开：
	// decision 是给事件流的语义，status 是给「同一条请求只能被处理一次」的条件更新用的。
	ApprovalPending = "pending"
	ApprovalAllowed = "allowed"
	ApprovalDenied  = "denied"
	ApprovalTimeout = "timeout"
	ApprovalExpired = "expired"
)

// ApprovalResolved 判断审批是否已有结论。
func ApprovalResolved(status string) bool {
	return status != "" && status != ApprovalPending
}

// 事件类型。前两类是高频流，落库前必须按时间窗口合并。
const (
	EventMessageDelta = "message_delta"
	EventThoughtDelta = "thought_delta"
	EventToolStart    = "tool_call_start"
	EventToolUpdate   = "tool_call_update"
	EventApprovalReq  = "approval_req"
	// EventApprovalResult 是人工审批的结论。与 approval_req 分开而不是原地改写：
	// 事件流是 append-only 的，前端靠「req(pending) → result」两跳恢复出审批时间线，
	// 刷新页面也能看到「当时是谁批的、隔了多久」。原地改写会把这个信息抹掉。
	EventApprovalResult = "approval_result"
	EventAgentLog       = "agent_log"
	EventError          = "error"
	EventState          = "run_state"
	// 多 agent：子任务派生与回话。分开两条是为了事件流 append-only——
	// 刷新页面也能看出「派生了谁、隔了多久才回话」。
	EventChildSpawned = "child_spawned"
	EventChildResult  = "child_result"
)

// IsTerminal 表示 run 已结束，不可再变。
func IsTerminal(status string) bool {
	switch status {
	case RunCompleted, RunFailed, RunCancelled:
		return true
	}
	return false
}

var (
	ErrNotFound = errors.New("store: 不存在")
	ErrConflict = errors.New("store: 状态冲突")
)

// Session 是一个会话。绑定关系：1 会话 = 1 沙箱 = 1 个 agent 进程。
type Session struct {
	ID string `json:"id"`
	// TenantID / UserID 是该会话的归属。空值只在「单租户兼容模式」下出现，
	// 一旦接入认证就必须填写——API 层按这两个字段过滤，缺了等于全泄漏。
	TenantID string `json:"tenantId,omitempty"`
	UserID   string `json:"userId,omitempty"`
	// SandboxID 与 ID 相同（M1 不做复用池），单独留字段是为了以后能换。
	SandboxID string `json:"sandboxId"`
	// AgentSessionID 是 ACP 的 sessionId，与平台会话 id 不是一回事。
	AgentSessionID string `json:"agentSessionId"`
	// Title 是展示用标题。首次发指令时由输入自动生成，用户改过之后不再覆盖。
	Title string `json:"title,omitempty"`
	// ParentSessionID 非空表示这是某个父会话派生的子 agent。
	// 只用于展示与级联清理，不改变隔离边界（子会话仍是独立沙箱）。
	ParentSessionID string    `json:"parentSessionId,omitempty"`
	Cwd             string    `json:"cwd"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"createdAt"`
	LastActiveAt    time.Time `json:"lastActiveAt"`
}

// Run 是一次 prompt 执行，也是事件流的归属单位。
type Run struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	// ParentRunID 指向派生这一轮的父 run。空表示用户直接发起的 run。
	ParentRunID string    `json:"parentRunId,omitempty"`
	Status      string    `json:"status"`
	Input       string    `json:"input"`
	StopReason  string    `json:"stopReason,omitempty"`
	Error       string    `json:"error,omitempty"`
	NextSeq     int64     `json:"nextSeq"`
	CreatedAt   time.Time `json:"createdAt"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	EndedAt     time.Time `json:"endedAt,omitempty"`
}

// Event 是一条已落库的事件。Seq 在 run 内单调连续，从 1 开始。
type Event struct {
	RunID     string          `json:"-"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Approval 是一条人工审批请求。
//
// 主键是 (RunID, ToolCallID) 而不是独立 id：等待方、HTTP 路由、事件里的
// toolCallId 都是这一对，另造一个 id 只会多一层「拿 id 换 id」。
//
// 为什么必须落库：等待审批的 agent 阻塞在沙箱里，控制面重启或多副本路由
// 一旦丢失这条记录，run 就永久停在 waiting_approval——界面上一直转圈，
// 用户唯一的出路是删会话。
type Approval struct {
	RunID      string    `json:"runId"`
	ToolCallID string    `json:"toolCallId"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	// ExpiresAt 到点按 timeout 处理。零值表示不设上限（当前不会出现）。
	ExpiresAt time.Time `json:"expiresAt"`
	// ResolvedAt / ResolvedBy 只在终态有意义。ResolvedBy 是 user / system。
	ResolvedAt time.Time `json:"resolvedAt,omitempty"`
	ResolvedBy string    `json:"resolvedBy,omitempty"`
}

// NewEvent 构造待追加的事件（Seq 由存储分配）。
type NewEvent struct {
	Type    string
	Payload any
}

// Store 是持久化接口。所有方法都要能在多 goroutine 下并发调用。
type Store interface {
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	ListSessions(ctx context.Context) ([]Session, error)
	PutSession(ctx context.Context, s Session) error
	DeleteSession(ctx context.Context, id string) error

	CreateRun(ctx context.Context, r Run) error
	GetRun(ctx context.Context, id string) (Run, error)
	ListRuns(ctx context.Context, sessionID string) ([]Run, error)

	// UpdateRun 覆盖写 run 元数据。状态跃迁的合法性由调用方（runner）保证，
	// 存储层不做状态机校验——那样会让重试和恢复变复杂。
	UpdateRun(ctx context.Context, r Run) error

	// Append 分配 seq 并追加事件。返回分配到的 seq。
	Append(ctx context.Context, runID string, e NewEvent) (Event, error)

	// Events 按 seq 升序返回 afterSeq 之后的事件。limit<=0 表示不限。
	Events(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error)

	Close() error
}

// ApprovalStore 是审批的持久化接口。
//
// 单独拆出来而不是塞进 Store：审批是「控制面多副本共享状态」这一新需求，
// 而 Store 是运行时会话存储的最小契约。合并会让只跑 reaper 的内存测试实现
// 被迫补五个用不到的方法（改测试的成本会转化成「以后没人敢加方法」）。
// runner 在 New 时断言 Store 同时实现了它，装配错误立刻暴露。
//
// 为什么审批必须落库：等待审批的 agent 阻塞在沙箱里。控制面重启或多副本路由
// 一旦丢失这条记录，run 就永久停在 waiting_approval——界面一直转圈，
// 用户唯一的出路是删会话。
type ApprovalStore interface {
	// CreateApproval 写入一条 pending 审批。同一 (run, toolCall) 重复写入是冲突。
	CreateApproval(ctx context.Context, a Approval) error

	// GetApproval 读一条审批。不存在返回 ErrNotFound。
	GetApproval(ctx context.Context, runID, toolCallID string) (Approval, error)

	// ListPendingApprovals 列出 pending 审批，runID 为空表示全部。
	// 按 created_at 升序，保证前端看到的顺序稳定。
	ListPendingApprovals(ctx context.Context, runID string) ([]Approval, error)

	// ResolveApproval 把 pending 条件更新成 status（allowed/denied/timeout）。
	//
	// 实现必须是原子的 `UPDATE ... WHERE status='pending'`：并发两次点击
	// 只能有一次命中，第二次返回 ErrConflict。审批是授权动作，重复生效
	// 等于越权，不能靠调用方加锁——多副本下那锁保护不了任何人。
	// 记录不存在返回 ErrNotFound。
	ResolveApproval(ctx context.Context, runID, toolCallID, status, resolvedBy string, at time.Time) error

	// ExpirePendingApprovals 把所有 pending 标成 expired，返回被改动的记录。
	// 控制面启动时调用：上一个进程里的等待方已经没了，这些请求不可能再被处理。
	ExpirePendingApprovals(ctx context.Context, at time.Time) ([]Approval, error)
}
