// Package collab 提供「一个 run 派生子 agent 并等它回话」的最小形态。
//
// 为什么是这个形态而不是群聊：Grok Bot 的多 bot 协作本质是「chief-of-staff
// 把任务拆给几个专家，各干各的，最后汇总」。对我们来说 80% 的价值就是
// 「父 run 能起一个独立沙箱跑子任务、拿到结果再继续」，不需要 agent 之间
// 自由发消息、也不需要新的事件总线原语。
//
// 一个子 agent = 一个新会话 = 一个新沙箱 = 一条新 ACP 连接。隔离沿用现有
// 边界（每会话独立 workspace / 凭据 / 出网归属），父子并发不共享 binding。
package collab

import (
	"context"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// SpawnRequest 派生一个子 agent。
// TenantID / UserID 由服务端从父 run 的会话继承，**不接受调用方传**——
// 否则等于允许往别人租户名下起沙箱。
type SpawnRequest struct {
	ParentRunID string
	Input       string
	Title       string
	TenantID    string
	UserID      string
}

// Child 是一条子任务记录（含结果与派生深度）。
type Child struct {
	SessionID   string    `json:"sessionId"`
	RunID       string    `json:"runId"`
	ParentRunID string    `json:"parentRunId"`
	Title       string    `json:"title,omitempty"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary,omitempty"`
	Error       string    `json:"error,omitempty"`
	Depth       int       `json:"depth"`
	CreatedAt   time.Time `json:"createdAt"`
	EndedAt     time.Time `json:"endedAt,omitempty"`
}

// Coordinator 是多 agent 能力的对外面。
type Coordinator interface {
	// Spawn 起子 agent 并**阻塞等到终态**（或超时），返回带 Summary 的记录。
	// 阻塞期间父 run 的事件流照常推送，不受影响。
	Spawn(ctx context.Context, req SpawnRequest) (Child, error)
	// List 返回某个父 run 派生的所有子任务（按创建时间正序）。
	List(ctx context.Context, parentRunID string) ([]Child, error)
	// Cancel 取消仍在跑的子任务。
	Cancel(ctx context.Context, runID string) error
	Close() error
}

// Sessions 是 collab 需要的会话能力（窄接口）。
//
// 刻意不直接 import runner：runner 已经 import 了 egress/connector，虽然
// collab → runner 目前不成环，但窄接口让这个包能独立测试，也避免哪天
// runner 想反过来用 collab 时立刻撞环。app 层做适配器。
type Sessions interface {
	CreateSession(ctx context.Context, opts CreateOpts) (SessionInfo, error)
	// StartRun 的 parentRunID 必须在 CreateRun 时就写进 store.Run，
	// 不能事后 GetRun+UpdateRun 补写：execute goroutine 同时在改 run，
	// 补写会被覆盖（或反过来把别人的字段顶掉）。
	StartRun(ctx context.Context, sessionID, input, parentRunID string) (RunInfo, error)
	CancelRun(ctx context.Context, runID string) error
}

type CreateOpts struct {
	ParentSessionID string
	TenantID        string
	UserID          string
	Title           string
}

type SessionInfo struct{ ID string }
type RunInfo struct{ ID string }

// Config 装配。
type Config struct {
	Sessions Sessions
	// DataDir 是 children.json 所在目录（原子写、0600）。
	DataDir string
	// MaxDepth 防止无限派生（子 agent 再派生子 agent）。0 用默认 2。
	MaxDepth int
	// Timeout 单个子任务的等待上限。0 用默认 5 分钟。
	Timeout time.Duration
}

// ErrTooDeep / ErrNotFound / ErrTimeout 由实现返回，API 层据此映射状态码。
var (
	ErrTooDeep  = errString("collab: 派生深度超限")
	ErrNotFound = errString("collab: 子任务不存在")
	ErrTimeout  = errString("collab: 子任务超时")
)

type errString string

func (e errString) Error() string { return string(e) }

// Open 装配协调器。实现见 coordinator.go（由子任务填充）。
// st 直接用 store.Store：它是叶子包，collab → store 不成环；测试用
// store.OpenFile(tempdir) 造真实 run/事件，比手写 mock 更接近线上。
func Open(cfg Config, st store.Store) (Coordinator, error) {
	return openImpl(cfg, st)
}
