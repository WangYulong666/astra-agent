// Package capability 是「我能干什么」的单一事实来源。
//
// 竞品收敛出的渐进式披露三层：name+description 常驻 → 命中读全文 → 资源按需。
// 这里对应 List(detail=false) / Get / List(detail=true)。
package capability

import "context"

// Spec 是一条原子能力的完整描述，由主控的 Sources 适配器汇总四类来源。
type Spec struct {
	Name        string
	Kind        string // api | cli | skill
	Description string
	Endpoint    string
	AuthAction  string
	Source      string // openapi | astra-cli | admin-cli | builtin | distilled
	RequiredEnv []string
	Requires    []string
	Enabled     bool
	Body        string // skill 的 SKILL.md 正文
}

// Item 是对外输出。detail=false 时 Endpoint/RequiredEnv/Requires/Body 一律留空。
type Item struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Source      string   `json:"source"`
	Endpoint    string   `json:"endpoint,omitempty"`
	AuthAction  string   `json:"authAction,omitempty"`
	RequiredEnv []string `json:"requiredEnv,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	Unavailable string   `json:"unavailable,omitempty"`
	Body        string   `json:"body,omitempty"`
}

type Sources interface {
	Collect(ctx context.Context) ([]Spec, error)
}

type Registry interface {
	List(ctx context.Context, detail bool) ([]Item, error)
	Get(ctx context.Context, name string) (Item, error)
	Refresh(ctx context.Context) error
}

// Open 装配注册表。实现见 registry.go（由并行任务填充）。
func Open(src Sources) (Registry, error) { return openImpl(src) }
