package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// 长期记忆（Milvus）接口。
//
// 安全不变式（platform-extension.md 第三节）：tenant_id + user_id 由服务端从
// 身份里取，客户端没有任何办法指定「读谁的记忆」——所以这里的方法签名里
// 刻意没有 userID 参数。任何想加这个参数的改动都应该被拒绝。

// MemoryIngest 写一条记忆。kind 只能是 fact / conversation，空值按 fact 处理。
func (c *Client) MemoryIngest(ctx context.Context, text, kind, sessionID string, meta map[string]any) (MemoryItem, error) {
	body := map[string]any{"text": text}
	if kind != "" {
		body["kind"] = kind
	}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	if len(meta) > 0 {
		body["meta"] = meta
	}
	var out MemoryItem
	err := c.do(ctx, http.MethodPost, "/v1/memory/ingest", nil, body, &out)
	return out, err
}

// MemorySearch 语义检索。k<=0 时用 10；服务端上限 50。
func (c *Client) MemorySearch(ctx context.Context, q string, k int, kind string) ([]MemoryItem, error) {
	if k <= 0 {
		k = 10
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("k", strconv.Itoa(k))
	if kind != "" {
		params.Set("kind", kind)
	}
	var out struct {
		Items []MemoryItem `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/memory/search", params, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []MemoryItem{}
	}
	return out.Items, nil
}

// MemoryStats 只读统计，不含内容。
func (c *Client) MemoryStats(ctx context.Context) (MemoryStats, error) {
	var out MemoryStats
	err := c.do(ctx, http.MethodGet, "/v1/memory/stats", nil, nil, &out)
	return out, err
}

// MemoryDelete 合规删除。只能删自己的（服务端按 tenant+user 过滤）。
func (c *Client) MemoryDelete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/memory/"+url.PathEscape(id), nil, nil, nil)
}
