// Package distill 把一次成功的 run **变成技能草稿**。
//
// 现状：skills/ 下的 SKILL.md 是人手写的。而平台其实已经存了做这件事所需的
// 全部素材——run 的工具调用序列、每步入参、最终回复、以及 Milvus 里的历史记忆。
// 缺的只是「轨迹 → 草稿 → 人确认」这条流水线。
//
// 刻意不用 LLM 生成正文：那样每次结果都不一样，而且容易写出没人能验证的步骤。
// 这里用**规则式抽取**——工具调用顺序、关键参数、文件路径都是事实，直接排版成
// 步骤清单，人只需要改措辞和补边界条件。稳定、可 diff、可审计。
package distill

import (
	"context"
	"errors"
	"time"
)

// Status 是草稿状态。
type Status string

const (
	StatusDraft    Status = "draft"
	StatusAccepted Status = "accepted"
	StatusRejected Status = "rejected"
)

// Step 是从事件流里还原出来的一步。
type Step struct {
	Index int    `json:"index"`
	Tool  string `json:"tool"`
	// Command 是 bash 的 command / write 的 file_path 这类最关键的那个参数。
	Command string `json:"command,omitempty"`
	// Summary 给这一行的一句话，来自工具入参里最有信息量的字段。
	Summary string `json:"summary,omitempty"`
	// Outcome 是 ok / failed / denied。
	Outcome string `json:"outcome,omitempty"`
}

// Draft 是一份待确认的技能草稿。
type Draft struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	RunID       string `json:"run_id"`
	SessionID   string `json:"session_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Body 是完整的 SKILL.md 内容（含 frontmatter），人改完直接能用。
	Body   string `json:"body"`
	Steps  []Step `json:"steps"`
	Status Status `json:"status"`
	// Path 是 accept 之后落地的位置（~/.astra/skills/<name>/SKILL.md）。
	Path      string    `json:"path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Distiller 是技能沉淀的对外面。
type Distiller interface {
	Enabled() bool
	// Distill 从一次 run 还原草稿。run 必须是终态，且至少有一次工具调用——
	// 纯问答沉淀不出可复用的步骤，硬生成只会是噪音。
	Distill(ctx context.Context, req Request) (Draft, error)
	List(ctx context.Context, tenantID string) ([]Draft, error)
	// Accept 把草稿写成 skill 目录，返回落地路径。同名已存在时覆盖前先备份。
	Accept(ctx context.Context, tenantID, id string) (string, error)
	Reject(ctx context.Context, tenantID, id string) error
}

// Request 是一次沉淀请求。
type Request struct {
	TenantID  string
	UserID    string
	RunID     string
	SessionID string
	// Name 技能名（slug）。空则由 run 的指令自动生成。
	Name string
}

// Config 装配。
type Config struct {
	// Enabled 关掉时 Open 返回 Disabled。
	Enabled bool
	// SkillDir accept 的落地根目录，默认 ~/.astra/skills。
	SkillDir string
	// MaxSteps 最多还原多少步，超出的丢弃并在正文里注明（默认 40）。
	MaxSteps int
}

var (
	ErrDisabled = errors.New("distill: 未启用")
	ErrNotFound = errors.New("distill: 草稿不存在")
)
