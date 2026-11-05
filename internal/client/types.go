package client

import (
	"encoding/json"
	"time"
)

// 这里的结构体与服务端 JSON 一一对应。字段名以 internal/store、
// internal/api、internal/memory 的实际 tag 为准（不猜、不重命名），
// 因为 CLI 的 --json 就是把服务端返回原样转出来，改一个 tag 就是破坏脚本兼容性。

// Session 会话（1 会话 = 1 沙箱 = 1 agent 进程）。
type Session struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenantId,omitempty"`
	UserID         string    `json:"userId,omitempty"`
	SandboxID      string    `json:"sandboxId"`
	AgentSessionID string    `json:"agentSessionId"`
	Title          string    `json:"title,omitempty"`
	Cwd            string    `json:"cwd"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"createdAt"`
	LastActiveAt   time.Time `json:"lastActiveAt"`
}

// Run 一次 prompt 执行。
type Run struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"sessionId"`
	Status     string    `json:"status"`
	Input      string    `json:"input"`
	StopReason string    `json:"stopReason,omitempty"`
	Error      string    `json:"error,omitempty"`
	NextSeq    int64     `json:"nextSeq"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	EndedAt    time.Time `json:"endedAt,omitempty"`
}

// 事件类型。与服务端 internal/store/types.go 的常量逐字对应。
const (
	EventMessageDelta   = "message_delta"
	EventThoughtDelta   = "thought_delta"
	EventToolStart      = "tool_call_start"
	EventToolUpdate     = "tool_call_update"
	EventApprovalReq    = "approval_req"
	EventApprovalResult = "approval_result"
	EventAgentLog       = "agent_log"
	EventError          = "error"
	EventState          = "run_state"

	// StreamOverflow 服务端缓冲区满时主动断开，提示客户端重连续传。
	StreamOverflow = "overflow"
	// StreamClientError 本地读流失败（不是服务端事件）。CLI 打成警告后退出。
	StreamClientError = "stream_error"
)

// Event 是一条事件流事件。
//
// SSE 的 id 就是 seq：服务端 writeEvent 把 store 里的 Seq 写进 id 字段，
// 断线重连时可以用 after_seq 续传，所以这两个值必须一起保留。
type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Text 取增量文本（message_delta / thought_delta 的 payload 是 {"text": "..."}）。
func (e Event) Text() string {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(e.Data, &p)
	return p.Text
}

// Status 取 run_state 的 status。
func (e Event) Status() string {
	var p struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(e.Data, &p)
	return p.Status
}

// Tool 取工具调用信息（tool_call_start / tool_call_update）。
// 两个事件的字段不完全一样（update 没有 rawInput），缺的留空即可。
func (e Event) Tool() ToolCall {
	var t ToolCall
	_ = json.Unmarshal(e.Data, &t)
	return t
}

// ErrMessage 取 error / agent_log 里的可读原因。
func (e Event) ErrMessage() string {
	var p struct {
		Error string `json:"error"`
		Kind  string `json:"kind"`
		Raw   string `json:"raw"`
	}
	_ = json.Unmarshal(e.Data, &p)
	switch {
	case p.Error != "":
		return p.Error
	case p.Kind != "":
		return p.Kind
	default:
		return p.Raw
	}
}

// IsTerminal 判断这条事件是否宣告 run 结束。
func (e Event) IsTerminal() bool {
	if e.Type != EventState {
		return false
	}
	switch e.Status() {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// ToolCall 工具调用。字段名与 manager.go dispatch 里写入的 payload 一致。
type ToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}

// FileEntry 目录项。
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// FileList 是 GET /v1/sessions/{id}/files 的响应。
type FileList struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileContent 是 GET /v1/sessions/{id}/file 的响应（预览，512KB 截断）。
type FileContent struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// MemoryItem 一条长期记忆。
type MemoryItem struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenant_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Kind      string         `json:"kind"`
	Text      string         `json:"text"`
	Score     float32        `json:"score,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// MemoryStats 记忆统计。
type MemoryStats struct {
	Enabled    bool   `json:"enabled"`
	Backend    string `json:"backend,omitempty"`
	Collection string `json:"collection,omitempty"`
	Count      int64  `json:"count"`
	Dim        int    `json:"dim,omitempty"`
	Embedder   string `json:"embedder,omitempty"`
}

// Approval 一条人工审批请求。
type Approval struct {
	RunID      string    `json:"runId"`
	ToolCallID string    `json:"toolCallId"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// ObsConfig 观测能力状态。secret key 永远不下发，这里也只有 public key。
type ObsConfig struct {
	Enabled      bool   `json:"enabled"`
	Endpoint     string `json:"endpoint"`
	PublicKey    string `json:"public_key"`
	DashboardURL string `json:"dashboard_url"`
}

// Trace 一条 trace 摘要（/v1/admin/obs/traces）。
type Trace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Status    string    `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	URL       string    `json:"url,omitempty"`
	Input     string    `json:"input,omitempty"`
}

// ── 运营侧 ───────────────────────────────────────────────────

// Tenant 租户。
type Tenant struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Slug              string    `json:"slug"`
	Status            string    `json:"status"`
	MonthlyTokenLimit int64     `json:"monthlyTokenLimit"`
	CreatedAt         time.Time `json:"createdAt"`
}

// User 用户。PasswordHash 服务端就已经 json:"-"，这里也不留字段。
type User struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenantId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	LastLoginAt time.Time `json:"lastLoginAt,omitempty"`
}

// Usage 一条用量明细。
type Usage struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenantId"`
	UserID       string    `json:"userId,omitempty"`
	SessionID    string    `json:"sessionId,omitempty"`
	RunID        string    `json:"runId,omitempty"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	CostMicroUSD int64     `json:"costMicroUsd"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"createdAt"`
}

// TotalTokens 入 + 出。
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// UsageSum 汇总。
type UsageSum struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	CostMicroUSD int64 `json:"costMicroUsd"`
	Calls        int64 `json:"calls"`
}

// TotalTokens 入 + 出。
func (s UsageSum) TotalTokens() int64 { return s.InputTokens + s.OutputTokens }

// UsageReport 是 /v1/admin/usage 的响应：明细 + 全时段汇总。
type UsageReport struct {
	Items []Usage  `json:"items"`
	Sum   UsageSum `json:"sum"`
}

// AuditLog 一条审计。
type AuditLog struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	UserID     string          `json:"userId,omitempty"`
	ActorEmail string          `json:"actorEmail,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"targetType"`
	TargetID   string          `json:"targetId,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	IP         string          `json:"ip,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Sandbox 一个运行中的沙箱（provider 视角 + 归属会话）。
type Sandbox struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	State        string    `json:"state"`
	Workspace    string    `json:"workspace,omitempty"`
	PID          int       `json:"pid,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	SessionID    string    `json:"sessionId,omitempty"`
	SessionTitle string    `json:"sessionTitle,omitempty"`
	TenantID     string    `json:"tenantId,omitempty"`
	UserID       string    `json:"userId,omitempty"`
}

// APIKey 长期凭据。明文只在创建时出现一次。
type APIKey struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenantId"`
	UserID     string    `json:"userId"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	RevokedAt  time.Time `json:"revokedAt,omitempty"`
}

// Revoked 判断是否已失效（过期与撤销都算）。与 store.APIKey.Revoked 同义。
func (k APIKey) Revoked() bool {
	if !k.RevokedAt.IsZero() {
		return true
	}
	return !k.ExpiresAt.IsZero() && time.Now().After(k.ExpiresAt)
}

// NewKey 是创建凭据的响应：key 元数据 + 只出现一次的明文。
type NewKey struct {
	Key       APIKey `json:"key"`
	Plaintext string `json:"plaintext"`
}

// LoginResult 登录响应。
type LoginResult struct {
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RefreshToken     string    `json:"refreshToken,omitempty"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitempty"`
	User             User      `json:"user"`
	Tenant           Tenant    `json:"tenant"`
}
