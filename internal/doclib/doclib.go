// Package doclib 是产品内置文档库。
//
// 两个用途、一份实现：
//   - 给用户看：使用指南、CLI 手册、部署说明，随二进制分发，离线可读；
//   - 给我们看：一篇**运行时生成**的「功能总览」，直接从能力注册表与 /v1/meta
//     推导当前有哪些能力、开没开。手写文档一定会和代码漂移，这一篇不会。
//
// 内容不 embed：docs/ 是人写的、经常改，embed 进二进制会让「改了文档要重新编译」
// 成为日常。改成和 astra-cli 一样的路子——make build 把 docs/ 拷到 bin/docs，
// app 启动时同步到 <dataDir>/docs，文档模块只读那个目录。
package doclib

import (
	"context"
	"errors"
	"time"
)

// Doc 是一篇文档。Index 返回时不带 Body（列表页不需要几千字正文）。
type Doc struct {
	Path      string    `json:"path"`     // 相对文档根的路径，如 "design/self-evolving-loop.md"
	Title     string    `json:"title"`    // 一级标题，没有就用文件名
	Category  string    `json:"category"` // guide | overview | design | adr | ops | status | history
	Summary   string    `json:"summary"`  // 首段纯文本，截断
	Body      string    `json:"body,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	Generated bool      `json:"generated,omitempty"` // 运行时生成（功能总览）
}

// Feature 是功能总览里的一行。
type Feature struct {
	Name        string
	Kind        string // api | cli | skill
	Description string
	Enabled     bool
	Source      string
	Unavailable string
}

// OverviewSource 由 app 层适配能力注册表，doclib 不反向依赖 capability。
type OverviewSource interface {
	Features(ctx context.Context) ([]Feature, error)
	// Meta 返回 provider / agent / policy / 各能力开关等键值，用于总览头部。
	Meta() map[string]string
}

type Config struct {
	// Root 是文档根目录（<dataDir>/docs）。为空时只有生成的总览可用。
	Root string
	// Overview 为 nil 时不出功能总览。
	Overview OverviewSource
	// OverviewPath 是生成文档的访问路径，默认 "overview"。
	OverviewPath string
}

type Library interface {
	// Index 列出全部文档（不含 Body），按分类与标题排序，生成的总览固定在最前。
	Index(ctx context.Context) ([]Doc, error)
	// Get 取单篇全文。生成的总览用 cfg.OverviewPath 访问。
	Get(ctx context.Context, path string) (Doc, error)
	// Search 在标题/摘要/正文里做大小写不敏感的子串匹配，返回按命中次数排序的摘要。
	Search(ctx context.Context, query string, limit int) ([]Doc, error)
}

var ErrNotFound = errors.New("doclib: 文档不存在")

// Open 装配文档库。实现见 library.go（由并行任务填充）。
func Open(cfg Config) (Library, error) { return openImpl(cfg) }
