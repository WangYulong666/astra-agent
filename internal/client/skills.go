package client

// 技能草稿（distill 产物）的人工闸门：列草稿 / accept / reject。
//
// 为什么需要它：accept 是自闭环里唯一「人」的环节，而在此之前它只有 HTTP 接口，
// 想放行一条草稿只能手写 curl —— 那就等于没有闸门，只有一道没人会去开的门。
//
// 本地 file 侧的 skills list/show/check/install 在 cmd/astra-cli/cmd_skills.go，
// 走的是 SKILL_DIR 目录；本文件走的是控制面。两者刻意分开：断网时前者仍然可用。

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SkillDraft 是一条待人工处理的技能草稿。字段与 internal/distill.Draft 对齐，
// 但不 import 那个包——CLI 不该被服务端内部结构绑死。
type SkillDraft struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Body        string    `json:"body"`
	Status      string    `json:"status"`
	Path        string    `json:"path,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SkillDrafts 列出草稿。limit <= 0 表示不限（服务端返回全部）。
func (c *Client) SkillDrafts(ctx context.Context, limit int) ([]SkillDraft, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	var out []SkillDraft
	if err := c.do(ctx, http.MethodGet, "/v1/skills/drafts", params, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []SkillDraft{}
	}
	return out, nil
}

// AcceptSkillDraft 把草稿落地成 skill，返回落盘路径（<SKILL_DIR>/<name>/SKILL.md）。
func (c *Client) AcceptSkillDraft(ctx context.Context, id string) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/skills/drafts/"+url.PathEscape(id)+"/accept", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// RejectSkillDraft 拒绝草稿（服务端返回 204，无响应体）。
func (c *Client) RejectSkillDraft(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/skills/drafts/"+url.PathEscape(id)+"/reject", nil, nil, nil)
}
