package obs

import (
	"strings"
	"testing"
)

// 归一化的目标只有一个：同一类路由在指标/日志里必须是同一个 label。
// 这里覆盖真实会出现的 id 形态，而不是教科书里的 uuid。
func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		// 真实路由（api.go / platform_routes.go）
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/metrics", "/metrics"},
		{"/v1/meta", "/v1/meta"},
		{"/v1/sessions", "/v1/sessions"},
		{"/v1/sessions/abc123", "/v1/sessions/:id"},
		{"/v1/sessions/abc123/runs", "/v1/sessions/:id/runs"},
		{"/v1/sessions/abc123/files", "/v1/sessions/:id/files"},
		{"/v1/sessions/abc123/file", "/v1/sessions/:id/file"},
		{"/v1/runs/01JABCDEFGHJKMNPQRSTVWXYZ0", "/v1/runs/:id"},
		{"/v1/runs/abc123/events", "/v1/runs/:id/events"},
		{"/v1/runs/abc123/history", "/v1/runs/:id/history"},
		{"/v1/runs/abc123/cancel", "/v1/runs/:id/cancel"},
		{"/v1/runs/abc123/approvals", "/v1/runs/:id/approvals"},
		{"/v1/runs/abc123/approvals/tool_1", "/v1/runs/:id/approvals/:id"},
		{"/v1/admin/tenants", "/v1/admin/tenants"},
		{"/v1/admin/tenants/t_1", "/v1/admin/tenants/:id"},
		{"/v1/admin/users/u_9f2", "/v1/admin/users/:id"},
		{"/v1/admin/apikeys/k-1", "/v1/admin/apikeys/:id"},
		{"/v1/admin/audit", "/v1/admin/audit"},
		{"/v1/usage/billing", "/v1/usage/billing"},
		{"/v1/sandboxes/sb-1", "/v1/sandboxes/:id"},
		{"/v1/sessions/abc123/llm-token", "/v1/sessions/:id/llm-token"},
		{"/v1/llm/v1/chat/completions", "/v1/llm/v1/chat/completions"},
		{"/v1/openapi.json", "/v1/openapi.json"},
		{"/docs", "/docs"},

		// Go 1.22 ServeMux 的模板形态（r.Pattern 直接传进来）
		{"/v1/sessions/{id}", "/v1/sessions/:id"},
		{"/v1/runs/{id}/approvals/{toolCallId}", "/v1/runs/:id/approvals/:id"},
		{"/v1/sessions/{id}/runs", "/v1/sessions/:id/runs"},

		// 各种 id 形态：纯数字、uuid、纯字母、超长、带点的
		{"/v1/sessions/12345", "/v1/sessions/:id"},
		{"/v1/sessions/9f1c2b3e-4d5a-6b7c-8d9e-0f1a2b3c4d5e", "/v1/sessions/:id"},
		{"/v1/runs/abcdefghijklmnopqrstuvwxyz", "/v1/runs/:id"},
		{"/v1/runs/1.2.3", "/v1/runs/:id"},

		// 边界
		{"", "/"},
		{"/", "/"},
		{"//v1//sessions//abc123//", "/v1/sessions/:id"},
		{"/v1/sessions/abc123?after_seq=5", "/v1/sessions/:id"},
		{"/v1/sessions/abc123/runs#frag", "/v1/sessions/:id/runs"},
		{"/assets/app.js", "/assets/:id"},
		{"/docs/architecture.html", "/docs/:id"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 归一化必须幂等：指标里已经存了归一化过的值，日志再处理一次不能变形。
func TestNormalizePathIdempotent(t *testing.T) {
	inputs := []string{
		"/v1/sessions/abc123/runs",
		"/v1/runs/{id}/approvals/{toolCallId}",
		"/v1/sessions/:id/files",
		"/",
	}
	for _, in := range inputs {
		once := NormalizePath(in)
		twice := NormalizePath(once)
		if once != twice {
			t.Errorf("非幂等：%q → %q → %q", in, once, twice)
		}
	}
}

// 基数护栏：任意 id 都不能出现在结果里。这条测试是给「以后有人加路由时
// 忘了登记静态词」准备的——最坏结果是多个 :id，而不是 label 泄漏。
func TestNormalizePathNoRawID(t *testing.T) {
	ids := []string{"abc123", "01JABCDEFGHJKMNPQRSTVWXYZ0", "u_9f2", "9f1c-4d5a"}
	for _, id := range ids {
		got := NormalizePath("/v1/sessions/" + id + "/runs")
		if strings.Contains(got, id) {
			t.Errorf("路径 %q 归一后仍含原始 id %q", got, id)
		}
		if got != "/v1/sessions/:id/runs" {
			t.Errorf("路径 %q 归一结果异常: %q", id, got)
		}
	}
}
