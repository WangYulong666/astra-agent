// Package skillhub 管 skill 的注册表、索引与 requires 校验。
//
// distill 产出草稿、人工 accept 落盘，但此前 accept 之后没有任何索引或通知，
// agent 与 CLI 都不知道多了技能——这是闭环最便宜也最关键的一处断点。
package skillhub

import (
	"context"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
)

type Skill struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	Requires    []string  `json:"requires"`
	Source      string    `json:"source"` // builtin | distilled
	Status      string    `json:"status"` // ready | unavailable
	Unavailable string    `json:"unavailable,omitempty"`
	Body        string    `json:"body,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Config struct {
	// SkillDir 是统一目录（SKILL_DIR，默认 ~/.astra/skills），distill 产物落这里。
	SkillDir string
	// BuiltinDir 是随发行版内置的 skills/，启动时同步进 SkillDir（不覆盖手改）。
	BuiltinDir string
	// CLIBin 用于校验 requires: [astra-cli]；空表示未找到。
	CLIBin string
	// Switches 是 /v1/meta 的能力开关，用于校验 requires 里的能力名。
	Switches map[string]bool
}

type Hub interface {
	List(ctx context.Context) ([]Skill, error)
	Get(ctx context.Context, name string) (Skill, error)
	Refresh(ctx context.Context) error
	Publish(ctx context.Context, name, body string, requires []string) (string, error)
}

// Open 装配。hub 非空时 accept/publish 会广播 skill_published 事件。
func Open(cfg Config, hub *eventbus.Hub) (Hub, error) { return openImpl(cfg, hub) }
