// Package memory 提供用户维度的长期记忆（向量检索）。
//
// 设计要点：
//   - 未配置 Milvus 时返回 Disabled 实现，平台照常启动——能力是「关掉」而不是「起不来」
//   - 所有 Read 路径强制带 tenant_id + user_id 过滤，跨用户读取在设计上不允许
//   - Embedding 可插拔：openai-compatible 端点，或 hash（仅开发用）
package memory

import (
	"context"
	"errors"
	"time"
)

// 记忆种类。conversation 由 run 结束时自动摘要写入，fact 由用户/agent 显式写入。
const (
	KindFact         = "fact"
	KindConversation = "conversation"
)

// Item 是一条记忆。
type Item struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenant_id"`
	UserID    string         `json:"user_id"`
	SessionID string         `json:"session_id,omitempty"`
	Kind      string         `json:"kind"`
	Text      string         `json:"text"`
	Score     float32        `json:"score,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// Query 检索条件。TenantID/UserID 由服务端从身份里取，不接受客户端传入。
type Query struct {
	TenantID string
	UserID   string
	Text     string
	K        int
	Kind     string
}

// Stats 集合概况（运营页用，不含内容）。
type Stats struct {
	Enabled    bool   `json:"enabled"`
	Backend    string `json:"backend"`
	Collection string `json:"collection"`
	Count      int64  `json:"count"`
	Dim        int    `json:"dim"`
	Embedder   string `json:"embedder"`
}

// Store 是记忆存储的抽象。Milvus 是唯一生产实现。
type Store interface {
	Ingest(ctx context.Context, it Item) (Item, error)
	Search(ctx context.Context, q Query) ([]Item, error)
	Delete(ctx context.Context, tenantID, userID, id string) error
	Stats(ctx context.Context) (Stats, error)
	Close() error
}

// ErrDisabled 表示该能力未启用。API 层据此返回 503 + 明确原因，而不是 500。
var ErrDisabled = errors.New("memory: 未启用（未配置 Milvus）")

// Config 装配 Milvus 与 Embedder。
type Config struct {
	Addr       string // MILVUS_ADDR，如 127.0.0.1:19530；空 = 未启用
	DB         string // 默认 default
	Collection string // 默认 astra_memory

	EmbedBaseURL string // 兼容 /embeddings 的端点；空则用 hash embedder（开发用）
	EmbedAPIKey  string
	EmbedModel   string
	Dim          int // 集合维度；0 表示按 embedder 探测
}

// Open 按配置返回实现。Addr 为空返回 Disabled。
func Open(cfg Config) (Store, error) {
	if cfg.Addr == "" {
		return Disabled(), nil
	}
	return openMilvus(cfg)
}

type disabled struct{}

func Disabled() Store { return disabled{} }

// IsDisabled 判断是否处于「未配置」状态。调用点据此决定是否提示用户。
func IsDisabled(s Store) bool {
	_, off := s.(disabled)
	return off
}

func (disabled) Ingest(context.Context, Item) (Item, error) { return Item{}, ErrDisabled }
func (disabled) Search(context.Context, Query) ([]Item, error) {
	return nil, ErrDisabled
}
func (disabled) Delete(context.Context, string, string, string) error { return ErrDisabled }
func (disabled) Stats(context.Context) (Stats, error) {
	return Stats{Enabled: false, Backend: "disabled"}, nil
}
func (disabled) Close() error { return nil }
