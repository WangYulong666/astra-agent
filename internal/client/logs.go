package client

// 日志 / 流水类查询。
//
// 为什么单独一个文件：运营侧后端把「能查历史」的端点散在好几处——
// 出网审计在 /v1/admin/egress/log、事件历史在 /v1/runs/{id}/history、
// 行动账本在 activity、用量流水在 /v1/usage。它们既不是租户治理也不是
// 会话管理，塞进 admin.go 或 api.go 都会让那两个文件继续膨胀。
// 方法只做协议翻译，输出格式交给 CLI。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// EgressLogEntry 是一条出网审计。字段与 internal/egress.LogEntry 对齐，
// 但不 import 那个包——client 是给外部程序用的，不该拖进服务端内部结构。
type EgressLogEntry struct {
	Time       time.Time `json:"time"`
	TenantID   string    `json:"tenant_id"`
	UserID     string    `json:"user_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	Host       string    `json:"host"`
	Port       string    `json:"port,omitempty"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason,omitempty"`
	BytesUp    int64     `json:"bytes_up,omitempty"`
	BytesDown  int64     `json:"bytes_down,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
}

// AdminEgressLog 查出网审计（按时间倒序）。tenant 为空表示只看自己租户，
// 跨租户需要服务端 audit:read 权限。
func (c *Client) AdminEgressLog(ctx context.Context, limit int, tenant string) ([]EgressLogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	if tenant != "" {
		params.Set("tenant", tenant)
	}
	var out struct {
		Items []EgressLogEntry `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/admin/egress/log", params, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []EgressLogEntry{}
	}
	return out.Items, nil
}

// ActivityEntry 是行动账本里的一条（用户视角，不是原始事件）。
type ActivityEntry struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	Status    string    `json:"status"`
	RunID     string    `json:"runId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	Ref       string    `json:"ref,omitempty"`
}

type ActivitySummary struct {
	Done    int    `json:"done"`
	Running int    `json:"running"`
	Blocked int    `json:"blocked"`
	Failed  int    `json:"failed"`
	Current string `json:"current,omitempty"`
}

// ActivityResult 同时容纳 run 与 session 两种响应。
// session 视图不返回 summary（后端按多个 run 聚合，计数由前端现算），
// 所以用指针区分「没有」与「全是 0」。
type ActivityResult struct {
	RunID     string           `json:"runId,omitempty"`
	SessionID string           `json:"sessionId,omitempty"`
	Entries   []ActivityEntry  `json:"entries"`
	Summary   *ActivitySummary `json:"summary,omitempty"`
}

func activityParams(limit int) url.Values {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	return params
}

func normalizeActivity(out *ActivityResult) {
	if out.Entries == nil {
		out.Entries = []ActivityEntry{}
	}
}

// RunActivity 查某个 run 的用户视角行动时间线。
func (c *Client) RunActivity(ctx context.Context, runID string, limit int) (ActivityResult, error) {
	if runID == "" {
		return ActivityResult{}, &UseError{Msg: "缺少 run id"}
	}
	var out ActivityResult
	path := fmt.Sprintf("/v1/runs/%s/activity", url.PathEscape(runID))
	if err := c.do(ctx, http.MethodGet, path, activityParams(limit), nil, &out); err != nil {
		return ActivityResult{}, err
	}
	normalizeActivity(&out)
	return out, nil
}

// SessionActivity 查某会话下所有 run 的聚合行动时间线。
func (c *Client) SessionActivity(ctx context.Context, sessionID string, limit int) (ActivityResult, error) {
	if sessionID == "" {
		return ActivityResult{}, &UseError{Msg: "缺少 session id"}
	}
	var out ActivityResult
	path := fmt.Sprintf("/v1/sessions/%s/activity", url.PathEscape(sessionID))
	if err := c.do(ctx, http.MethodGet, path, activityParams(limit), nil, &out); err != nil {
		return ActivityResult{}, err
	}
	normalizeActivity(&out)
	return out, nil
}

// ChildRun 是一条多 agent 派生记录。
type ChildRun struct {
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

// RunChildren 列出某父 run 派生的所有子任务。
func (c *Client) RunChildren(ctx context.Context, runID string) ([]ChildRun, error) {
	if runID == "" {
		return nil, &UseError{Msg: "缺少 run id"}
	}
	path := fmt.Sprintf("/v1/runs/%s/children", url.PathEscape(runID))
	var out []ChildRun
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []ChildRun{}
	}
	return out, nil
}

// Usage 查本租户用量明细。与 AdminUsage 的区别：这个走 /v1/usage，
// 只需要 usage:read（成员也能查自己的），不是全租户治理视角。
func (c *Client) Usage(ctx context.Context, q UsageQuery) (UsageReport, error) {
	params := url.Values{}
	if q.UserID != "" {
		params.Set("userId", q.UserID)
	}
	if q.From != "" {
		params.Set("from", q.From)
	}
	if q.To != "" {
		params.Set("to", q.To)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out UsageReport
	err := c.do(ctx, http.MethodGet, "/v1/usage", params, nil, &out)
	return out, err
}

// Billing 查本月账单。响应形状由服务端定价配置决定，这里保持原始 map，
// 避免 client 侧硬编码一套随时会变的计费字段。
func (c *Client) Billing(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/v1/usage/billing", nil, nil, &out)
	return out, err
}

// Insight 是后端 GET /v1/insights 返回的一条失败 run 结构化诊断。
// 字段与 internal/insight.Diagnosis 对齐，但不 import 服务端包——client 是给
// 外部程序用的，只认 JSON 形状。CLI 的「回灌下一轮」不在这里做，这里只翻译协议。
type Insight struct {
	RunID      string    `json:"runId"`
	SessionID  string    `json:"sessionId"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	LastTool   string    `json:"lastTool,omitempty"`
	Suggestion string    `json:"suggestion"`
	ToolCount  int       `json:"toolCount"`
	Blocked    int       `json:"blocked"`
	Failed     int       `json:"failed"`
	CreatedAt  time.Time `json:"createdAt"`
}

// AdminInsights 查失败诊断（按时间倒序）。sessionID 非空时按会话过滤，
// 留空查全租户（需要 owner/admin）。limit<=0 用服务端默认 50，上限 500。
// 权限与 /v1/insights 一致：需要 audit:read。
func (c *Client) AdminInsights(ctx context.Context, sessionID string, limit int) ([]Insight, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	if sessionID != "" {
		params.Set("session_id", sessionID)
	}
	var out struct {
		Items []Insight `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/insights", params, nil, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Insight{}
	}
	return out.Items, nil
}
