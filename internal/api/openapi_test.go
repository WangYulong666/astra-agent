package api

// 契约防漂移测试。
//
// 三件事，缺一不可：
//  1. 契约本身能解析，且每个操作都补齐了 operationId / summary / tags
//     —— 少一个，前端或 SDK 生成器就会拿到一个没有名字的接口；
//  2. yaml 里声明的每个 path+method 在真实 Handler() 上都能命中
//     —— 只会往一个方向漂：文档写了、代码没有（或路径拼错）；
//  3. 代码注册的路由集合与 yaml 的集合完全相等
//     —— 反向漂移：代码加了接口、文档忘了写。
//
// 为什么 2 和 3 都要做：
//   ServeMux 注册了 `GET /`（前端 SPA 兜底），任何未注册的 GET 路径都会拿到
//   200 + index.html——只看状态码会误判「路由存在」。所以 2 额外检查 GET 不能
//   落到兜底（返回 text/html），3 用集合比对补上另一个方向。
//
// 为什么这里手抄一份路由清单而不是反射：ServeMux 不暴露已注册的模式。
// 手抄的清单如果抄错了，测试 2 会立刻失败（请求打不到），两个方向互为校验。

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// codeRoutes 是 API 层实际注册的全部路由（api.go + platform_routes.go + setup.go
// 的并集）。
//
// 只有两处刻意的改写：代码里是 `POST /v1/llm/{path...}`（后端通配，要能收
// OpenAI 的任意子路径），OpenAPI 用单段参数 `/v1/llm/{path}` 表达同一件事；
// 同理 GET / DELETE 也注册在同一条通配上，一并展开列出。
// 静态资源兜底 `GET /` 不属于对外契约，不列入。
//
// 这份清单不能漂：TestCodeRoutesListMatchesSource 会扫源码里的注册字面量
// 与它比对，抄漏一条测试立刻红。
var codeRoutes = []string{
	"GET /healthz",
	"GET /readyz",
	"GET /metrics",

	"POST /v1/auth/bootstrap",
	"POST /v1/auth/login",
	"POST /v1/auth/logout",
	"POST /v1/auth/refresh",
	"POST /v1/auth/sse-ticket",
	"GET /v1/auth/me",
	"PATCH /v1/auth/password",

	"GET /v1/setup/state",
	"POST /v1/setup/complete",

	"GET /v1/meta",

	"POST /v1/sessions",
	"GET /v1/sessions",
	"GET /v1/sessions/{id}",
	"PATCH /v1/sessions/{id}",
	"DELETE /v1/sessions/{id}",
	"GET /v1/sessions/{id}/files",
	"GET /v1/sessions/{id}/file",
	"GET /v1/sessions/{id}/download",
	"POST /v1/sessions/{id}/runs",
	"GET /v1/sessions/{id}/runs",
	"POST /v1/sessions/{id}/llm-token",

	"GET /v1/runs/{id}",
	"POST /v1/runs/{id}/cancel",
	"GET /v1/runs/{id}/events",
	"GET /v1/runs/{id}/history",
	"GET /v1/runs/{id}/approvals",
	"POST /v1/runs/{id}/approvals/{toolCallId}",

	"GET /v1/sandboxes",
	"DELETE /v1/sandboxes/{id}",

	"GET /v1/usage",
	"GET /v1/usage/billing",

	"GET /v1/admin/tenants",
	"POST /v1/admin/tenants",
	"GET /v1/admin/tenants/{id}",
	"PATCH /v1/admin/tenants/{id}",
	"GET /v1/admin/users",
	"POST /v1/admin/users",
	"PATCH /v1/admin/users/{id}",
	"GET /v1/admin/apikeys",
	"POST /v1/admin/apikeys",
	"DELETE /v1/admin/apikeys/{id}",
	"GET /v1/admin/audit",
	"GET /v1/admin/usage",
	"GET /v1/admin/llm-settings",
	"POST /v1/admin/llm-settings",
	"POST /v1/admin/llm-settings/test",
	"GET /v1/admin/lifecycle",
	"POST /v1/admin/lifecycle/sweep",

	// 长期记忆：全部按登录身份的 tenant+user 过滤，不接受请求体传用户
	"POST /v1/memory/ingest",
	"GET /v1/memory/search",
	"GET /v1/memory/stats",
	"DELETE /v1/memory/{id}",

	// 观测：运营页反查 trace（secret key 留服务端）
	"GET /v1/admin/obs/config",
	"GET /v1/admin/obs/traces",

	// 出网管控：策略读写与审计日志（读写权限分开）
	"GET /v1/admin/egress/policy",
	"PUT /v1/admin/egress/policy",
	"GET /v1/admin/egress/log",

	// 调度：cron / webhook 触发，让 agent 自己启动
	"GET /v1/schedules",
	"POST /v1/schedules",
	"GET /v1/schedules/{id}",
	"PATCH /v1/schedules/{id}",
	"DELETE /v1/schedules/{id}",
	"POST /v1/schedules/{id}/run",
	"POST /v1/hooks/{token}",

	// 技能沉淀：轨迹 → 草稿 → 人工确认才落地
	"POST /v1/runs/{id}/distill",
	"GET /v1/skills/drafts",
	"POST /v1/skills/drafts/{id}/accept",
	"POST /v1/skills/drafts/{id}/reject",

	// 连接器：OAuth / API key 托管。callback 由浏览器直接访问，靠 state 鉴权
	"GET /v1/connectors",
	"POST /v1/connectors/{name}/connect",
	"POST /v1/connectors/{name}/key",
	"DELETE /v1/connectors/{name}",
	"GET /v1/connectors/callback",

	// 多 agent 协作：父 run 派生子 agent（归属由服务端从父会话继承）
	"POST /v1/runs/{id}/children",
	"GET /v1/runs/{id}/children",

	// 行动账本：用户视角的「做了什么、卡在哪」
	"GET /v1/runs/{id}/activity",
	"GET /v1/sessions/{id}/activity",

	// 文档模块（只读，随二进制分发）
	"GET /v1/docs",
	"GET /v1/docs/search",
	"GET /v1/docs/{path...}",

	// 自闭环：能力发现 / 技能注册 / 失败诊断
	"GET /v1/capabilities",
	"GET /v1/capabilities/{name}",
	"GET /v1/skills",
	"GET /v1/skills/{name}",
	"GET /v1/insights",

	// 浏览器：每会话一个隔离的 headless Chrome 实例
	"POST /v1/sessions/{id}/browser",
	"GET /v1/sessions/{id}/browser",
	"DELETE /v1/sessions/{id}/browser",

	"GET /v1/llm/{path}",
	"POST /v1/llm/{path}",
	"DELETE /v1/llm/{path}",

	"GET /v1/openapi.json",
	"GET /docs",
}

// requiredSchemas 是本期契约必须定义的类型。少一个就意味着某个接口的
// 请求体或响应体只能写成 additionalProperties，前端无从生成类型。
var requiredSchemas = []string{
	"Session", "Run", "Event", "Approval", "Meta", "Tenant", "User", "APIKey",
	"Usage", "UsageSum", "Billing", "AuditLog", "Sandbox", "FileEntry",
	"LoginRequest", "LoginResponse", "BootstrapRequest", "Error",
	"SetupState", "SetupCompleteRequest", "SetupCompleteResponse",
	"LLMSettingsView", "UpdateLLMSettingsRequest", "LLMSettingsTestRequest", "LLMSettingsTestResult",
	"LifecyclePolicyView", "LifecycleReportView", "LifecycleStatusView",
}

// httpMethods 是 OpenAPI 里允许出现在 path item 下的操作键。
var httpMethods = []string{"get", "post", "put", "patch", "delete", "head", "options"}

// loadSpec 解析内嵌的契约。刻意走 openapiDocument 的同一条解析路径：
// 如果解析器坏了，测试必须跟着红，而不是另起一条「测试专用」的捷径。
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	doc, err := parseYAMLSubset(openapiYAML)
	if err != nil {
		t.Fatalf("契约解析失败: %v", err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("契约顶层不是映射，实际是 %T", doc)
	}
	return m
}

func specPaths(t *testing.T, spec map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("契约缺少 paths")
	}
	out := make(map[string]map[string]any, len(raw))
	for p, item := range raw {
		ops, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("paths.%s 不是映射", p)
		}
		out[p] = ops
	}
	return out
}

// TestOpenAPISpecShape 校验契约的骨架与元信息。
func TestOpenAPISpecShape(t *testing.T) {
	spec := loadSpec(t)

	if got := spec["openapi"]; got != "3.1.0" {
		t.Errorf("openapi = %v，期望 3.1.0（字符串，不能是数字）", got)
	}
	info, _ := spec["info"].(map[string]any)
	if info["title"] != "astra-agent API" {
		t.Errorf("info.title = %v，期望 astra-agent API", info["title"])
	}
	if info["version"] != "1.0.0" {
		t.Errorf("info.version = %v，期望 1.0.0", info["version"])
	}

	servers, _ := spec["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers 期望 1 条，实际 %d", len(servers))
	}
	if s, _ := servers[0].(map[string]any); s["url"] != "/" {
		t.Errorf("servers[0].url = %v，期望 /", s["url"])
	}

	// 全局 security 必须同时接受 JWT 与 API Key，否则 API Key 客户端会被判成匿名。
	global, _ := spec["security"].([]any)
	if len(global) != 2 {
		t.Fatalf("全局 security 期望 2 个方案，实际 %d", len(global))
	}
	names := map[string]bool{}
	for _, g := range global {
		for k := range g.(map[string]any) {
			names[k] = true
		}
	}
	for _, want := range []string{"bearerAuth", "apiKeyAuth"} {
		if !names[want] {
			t.Errorf("全局 security 缺少 %s", want)
		}
	}

	comps, _ := spec["components"].(map[string]any)
	schemes, _ := comps["securitySchemes"].(map[string]any)
	bearer, _ := schemes["bearerAuth"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Errorf("bearerAuth 必须是 http/bearer，实际 %v", bearer)
	}
	apiKey, _ := schemes["apiKeyAuth"].(map[string]any)
	if apiKey["type"] != "apiKey" || apiKey["in"] != "header" || apiKey["name"] != "X-API-Key" {
		t.Errorf("apiKeyAuth 必须是 header 里的 X-API-Key，实际 %v", apiKey)
	}

	schemas, _ := comps["schemas"].(map[string]any)
	for _, name := range requiredSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("components.schemas 缺少 %s", name)
		}
	}

	// 公开接口必须显式 security: []，否则会被默认要求带凭据。
	paths := specPaths(t, spec)
	for _, pub := range []string{"/healthz", "/readyz", "/metrics", "/v1/auth/login", "/v1/auth/bootstrap",
		"/v1/setup/state", "/v1/setup/complete", "/docs"} {
		item, ok := paths[pub]
		if !ok {
			t.Errorf("缺少公开接口 %s", pub)
			continue
		}
		for m, raw := range item {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			sec, exists := op["security"]
			if !exists {
				t.Errorf("%s %s 缺少 security: []（公开接口必须显式声明）", strings.ToUpper(m), pub)
				continue
			}
			if list, ok := sec.([]any); !ok || len(list) != 0 {
				t.Errorf("%s %s 的 security 必须是空数组", strings.ToUpper(m), pub)
			}
		}
	}
}

// TestOpenAPIOperationsDocumented 保证每个操作都能被 SDK 生成器用起来。
func TestOpenAPIOperationsDocumented(t *testing.T) {
	spec := loadSpec(t)
	paths := specPaths(t, spec)

	seenID := map[string]string{}
	var ids []string
	for p, item := range paths {
		for m, raw := range item {
			if !contains(httpMethods, m) {
				continue
			}
			op, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s 不是映射", strings.ToUpper(m), p)
			}
			where := strings.ToUpper(m) + " " + p
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s 缺少 operationId", where)
			} else {
				if strings.Contains(id, "-") {
					t.Errorf("%s 的 operationId %q 用了连字符，约定是下划线", where, id)
				}
				if prev, dup := seenID[id]; dup {
					t.Errorf("operationId %q 重复：%s 与 %s", id, prev, where)
				}
				seenID[id] = where
				ids = append(ids, id)
			}
			if s, _ := op["summary"].(string); s == "" {
				t.Errorf("%s 缺少 summary", where)
			}
			tags, _ := op["tags"].([]any)
			if len(tags) == 0 {
				t.Errorf("%s 缺少 tags（浏览页靠它分组）", where)
			}
			if _, ok := op["responses"].(map[string]any); !ok {
				t.Errorf("%s 缺少 responses", where)
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("契约里一个操作都没有")
	}
	sort.Strings(ids)
}

// TestOpenAPISchemaMatchesGoStructs 是最关键的一条：schema 字段名必须与
// Go struct 的 json tag 完全一致。
//
// 做法是拿「填满所有字段的真实结构体」marshal 出来的键集合去比对，
// 而不是手抄一份期望值——手抄的期望值会跟着一起错。
// 两个方向都比：schema 少了字段 → 前端拿不到；schema 多了字段 → 前端用了一个
// 服务端永远不会返回的字段（更隐蔽）。
func TestOpenAPISchemaMatchesGoStructs(t *testing.T) {
	spec := loadSpec(t)
	comps, _ := spec["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)

	now := time.Now().UTC()

	for _, c := range schemaFixtures(now) {
		t.Run(c.schema, func(t *testing.T) {
			raw, ok := schemas[c.schema].(map[string]any)
			if !ok {
				t.Fatalf("schema %s 不存在", c.schema)
			}
			props, _ := raw["properties"].(map[string]any)
			if len(props) == 0 {
				t.Fatalf("schema %s 没有 properties", c.schema)
			}
			got := jsonKeys(t, c.value)
			for k := range got {
				if _, ok := props[k]; !ok {
					t.Errorf("schema %s 缺字段 %q（Go struct 会输出它）", c.schema, k)
				}
			}
			for k := range props {
				if !got[k] {
					t.Errorf("schema %s 多了字段 %q（Go struct 的 json tag 里没有）", c.schema, k)
				}
			}
		})
	}
}

// TestOpenAPIRoutesMatchCode 是反向漂移：代码与契约的路由集合必须相等。
func TestOpenAPIRoutesMatchCode(t *testing.T) {
	spec := loadSpec(t)
	paths := specPaths(t, spec)

	inSpec := map[string]bool{}
	for p, item := range paths {
		for m := range item {
			if !contains(httpMethods, m) {
				continue
			}
			inSpec[strings.ToUpper(m)+" "+p] = true
		}
	}
	inCode := map[string]bool{}
	for _, r := range codeRoutes {
		inCode[r] = true
	}

	for _, r := range sortedKeys(inCode) {
		if !inSpec[r] {
			t.Errorf("yaml 缺了 %s（代码里已注册）", r)
		}
	}
	for _, r := range sortedKeys(inSpec) {
		if !inCode[r] {
			t.Errorf("yaml 多了 %s（代码里没有注册，或路径拼错）", r)
		}
	}
	if t.Failed() {
		t.Logf("对照：代码 %d 条，契约 %d 条", len(inCode), len(inSpec))
	}
}

// TestCodeRoutesListMatchesSource 堵住最后一条漂移缝：codeRoutes 本身是
// 手抄的，抄漏一条的话 TestOpenAPIRoutesMatchCode 只会比出「两边相等」的假绿。
//
// 做法是用 go/parser 扫包内非测试源码里 mux.HandleFunc / mux.Handle 的
// 首个参数字面量，与 codeRoutes 做集合比对。它不替代运行时校验
// （AST 拿不到真实注册的方法集与路径优先级），只保证「抄清单」这一步不漂。
func TestCodeRoutesListMatchesSource(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录: %v", err)
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			switch arg := call.Args[0].(type) {
			case *ast.BasicLit:
				s := strings.Trim(arg.Value, "\"`")
				if s == "GET /" {
					return true // SPA 静态兜底，不属于对外契约
				}
				if isRoutePattern(s) {
					found = append(found, s)
				}
			case *ast.BinaryExpr:
				// 方法来自循环变量的通配路由（platform_routes.go 的 LLM 透传）：
				//   for _, m := range []string{"GET","POST","DELETE"} {
				//       mux.HandleFunc(m+" /v1/llm/{path...}", ...)
				//   }
				// AST 只看得到右操作数字面量，按注册处的真实方法集展开。
				lit, ok := arg.Y.(*ast.BasicLit)
				if !ok {
					return true
				}
				p := strings.Trim(lit.Value, "\"`")
				if !strings.HasSuffix(p, "/{path...}") {
					return true
				}
				for _, m := range []string{"GET", "POST", "DELETE"} {
					found = append(found, m+" /v1/llm/{path}")
				}
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("源码里一条路由注册都没扫到——扫描逻辑本身坏了，先修它")
	}

	inSrc := map[string]bool{}
	for _, r := range found {
		inSrc[r] = true
	}
	inList := map[string]bool{}
	for _, r := range codeRoutes {
		inList[r] = true
	}
	for _, r := range sortedKeys(inSrc) {
		if !inList[r] {
			t.Errorf("源码注册了 %s，但 codeRoutes 没抄——手抄清单漏了", r)
		}
	}
	for _, r := range sortedKeys(inList) {
		if !inSrc[r] {
			t.Errorf("codeRoutes 里有 %s，但源码里没有注册（路由删了清单没删，或清单抄错）", r)
		}
	}
}

// isRoutePattern 判断字符串是不是「METHOD /path」形状的路由模式。
func isRoutePattern(s string) bool {
	i := strings.IndexByte(s, ' ')
	if i <= 0 {
		return false
	}
	switch s[:i] {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return false
	}
	return strings.HasPrefix(s[i+1:], "/")
}

// TestOpenAPIEveryPathIsRouted 把契约里每一条都真请求一遍。
//
// 兼容模式（Config.Auth == nil）下所有接口放行，所以不带凭据也能进 handler；
// 治理层未装配时多数接口回 501，这正是我们要的「路由存在」信号。
func TestOpenAPIEveryPathIsRouted(t *testing.T) {
	h := newHarness(t)

	// 用真实资源填 path 参数：假 id 只会拿到 handler 的 404，与「路由不存在」
	// 混在一起就没法区分了。
	sess := h.newSession(t)
	doomed := h.newSession(t) // 专门留给 DELETE 用，避免删掉后面要用的会话

	code, body := h.do(t, http.MethodPost, "/v1/sessions/"+sess.ID+"/runs", map[string]any{"input": "openapi 路由探针"})
	if code != http.StatusCreated {
		t.Fatalf("建 run 返回 %d: %s", code, body)
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatalf("run 响应不是 JSON: %v", err)
	}

	// 等 run 结束：SSE 对未结束的 run 会一直挂着，探针不该为了一个 GET 等几分钟。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := h.st.GetRun(t.Context(), run.ID)
		if err == nil && isTerminalForTest(cur.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	spec := loadSpec(t)
	paths := specPaths(t, spec)
	client := &http.Client{Timeout: 15 * time.Second}

	var checked int
	for p, item := range paths {
		for m := range item {
			if !contains(httpMethods, m) {
				continue
			}
			method := strings.ToUpper(m)
			target, ok := probeURL(p, method, sess.ID, doomed.ID, run.ID)
			if !ok {
				t.Errorf("%s %s：测试没有为这个路径准备参数（改契约后忘了改测试）", method, p)
				continue
			}
			req, err := http.NewRequest(method, h.srv.URL+target, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("%s %s: %v", method, target, err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, target, err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			checked++

			switch {
			case resp.StatusCode == http.StatusMethodNotAllowed:
				t.Errorf("%s %s 返回 405：这条路有别的 method，契约写错了", method, p)
			case resp.StatusCode == http.StatusNotFound && !isHandlerJSON(resp.Header, raw):
				// ServeMux 的 404 是纯文本 "404 page not found"；handler 的 404 是 {"error":...}。
				// 前者才是「这条路由不存在」。
				t.Errorf("%s %s 返回 404：路由没注册（契约漏写了代码，或路径拼错）", method, p)
			case method == http.MethodGet && p != "/docs" &&
				resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html"):
				// 被 SPA 兜底 `GET /` 接住了：状态码 200 但根本没有对应 handler。
				t.Errorf("GET %s 落到了前端兜底（返回 HTML），说明路由没注册", p)
			}
		}
	}
	if want := len(codeRoutes); checked != want {
		t.Errorf("只探了 %d 条路由，代码注册了 %d 条", checked, want)
	}
}

// TestOpenAPIEndpoints 验证两个出口本身：JSON 可解析、浏览页是自包含 HTML。
func TestOpenAPIEndpoints(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(t, http.MethodGet, "/v1/openapi.json", nil)
	if code != http.StatusOK {
		t.Fatalf("/v1/openapi.json 返回 %d: %s", code, body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("导出的内容不是合法 JSON: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Errorf("导出的 openapi = %v", doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Error("导出的文档没有 paths")
	}

	resp, err := http.Get(h.srv.URL + "/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/docs 返回 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/docs Content-Type = %q", ct)
	}
	html := string(page)
	for _, want := range []string{"/v1/openapi.json", "prefers-color-scheme"} {
		if !strings.Contains(html, want) {
			t.Errorf("/docs 页面里找不到 %q", want)
		}
	}
	// 离线可用：不能引外部 CDN。
	for _, bad := range []string{"http://", "https://", "cdn.", "<script src="} {
		if strings.Contains(html, bad) {
			t.Errorf("/docs 引用了外部资源 %q，离线环境会白屏", bad)
		}
	}
}

// ── 探针辅助 ─────────────────────────────────────────────────

const probeDummyID = "0000000000000000000000000"

// probeURL 把契约里的 path 模板换成可请求的真实路径。
//
// 返回 false 表示测试不认识这个模板——契约加了新路径就必须同步改这里，
// 否则等于悄悄放行（这正是漂移的一种）。
func probeURL(p, method, sessID, doomedID, runID string) (string, bool) {
	switch {
	case p == "/v1/llm/{path}":
		return "/v1/llm/v1/chat/completions", true
	case strings.HasPrefix(p, "/v1/runs/{id}"):
		return strings.NewReplacer("{id}", runID, "{toolCallId}", "tc-openapi-probe").Replace(p), true
	case strings.HasPrefix(p, "/v1/sessions/{id}"):
		sid := sessID
		if method == http.MethodDelete && p == "/v1/sessions/{id}" {
			sid = doomedID
		}
		return strings.ReplaceAll(p, "{id}", sid), true
	case p == "/v1/sandboxes/{id}", strings.HasPrefix(p, "/v1/admin/"):
		return strings.ReplaceAll(p, "{id}", probeDummyID), true
	// 记忆的 {id} 用假 id：记忆未启用时 handler 回 503，启用时回 404，
	// 两种都算「路由存在」，探针只关心这一点。
	case strings.HasPrefix(p, "/v1/memory/"):
		return strings.ReplaceAll(p, "{id}", probeDummyID), true
	// 新版能力：能力关闭时 handler 回 503 JSON，探针只关心「路由存在」，
	// 所以占位符填个不会被误认成真资源的假值即可。
	case strings.HasPrefix(p, "/v1/schedules/"), strings.HasPrefix(p, "/v1/skills/drafts/"):
		return strings.ReplaceAll(p, "{id}", probeDummyID), true
	case p == "/v1/hooks/{token}":
		return "/v1/hooks/probe-token", true
	case strings.HasPrefix(p, "/v1/connectors/"):
		return strings.ReplaceAll(p, "{name}", "github"), true
	// 能力/技能单项查询的 {name}：能力未启用时 handler 回 503，启用时回 404，
	// 两种都算「路由存在」。
	// 文档路径是 {path...} 通配，探针填一个生成文档的路径即可。
	case strings.HasPrefix(p, "/v1/docs/"):
		return strings.ReplaceAll(p, "{path...}", "overview"), true
	case strings.HasPrefix(p, "/v1/capabilities/"), p == "/v1/skills/{name}":
		return strings.ReplaceAll(p, "{name}", probeDummyID), true
	}
	// 不含参数的路径；含 { 说明有未处理的模板。
	return p, !strings.Contains(p, "{")
}

// isHandlerJSON 区分「handler 主动回的 404」与「ServeMux 的 404」。
func isHandlerJSON(h http.Header, body []byte) bool {
	if !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		return false
	}
	var e map[string]any
	return json.Unmarshal(body, &e) == nil
}

func isTerminalForTest(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// ── 结构体样本 ──────────────────────────────────────────────
//
// 每个字段都要填：omitempty 的字段一旦是零值就不会出现在 JSON 里，
// 于是「schema 少了一个可空字段」这种漂移会被漏掉。

func schemaFixtures(now time.Time) []struct {
	schema string
	value  any
} {
	return []struct {
		schema string
		value  any
	}{
		{"Session", store.Session{
			ID: "s1", TenantID: "t1", UserID: "u1", SandboxID: "s1",
			AgentSessionID: "a1", Title: "标题", Cwd: "/w", Status: store.SessionActive,
			CreatedAt: now, LastActiveAt: now,
		}},
		{"Run", store.Run{
			ID: "r1", SessionID: "s1", Status: store.RunRunning, Input: "hi",
			StopReason: "end_turn", Error: "e", NextSeq: 3,
			CreatedAt: now, StartedAt: now, EndedAt: now,
		}},
		{"Event", store.Event{
			RunID: "r1", Seq: 1, Type: store.EventMessageDelta,
			Payload: json.RawMessage(`{}`), CreatedAt: now,
		}},
		{"Tenant", store.Tenant{
			ID: "t1", Name: "租户", Slug: "acme", Status: store.StatusActive,
			MonthlyTokenLimit: 100, CreatedAt: now,
		}},
		{"User", store.User{
			ID: "u1", TenantID: "t1", Email: "a@b.com", DisplayName: "甲",
			PasswordHash: "h", Role: store.RoleOwner, Status: store.StatusActive,
			CreatedAt: now, LastLoginAt: now,
		}},
		{"APIKey", store.APIKey{
			ID: "k1", TenantID: "t1", UserID: "u1", Name: "ci",
			Prefix: "astra_sk_a1b2c3", KeyHash: "h",
			CreatedAt: now, LastUsedAt: now, ExpiresAt: now, RevokedAt: now,
		}},
		{"Usage", store.Usage{
			ID: "g1", TenantID: "t1", UserID: "u1", SessionID: "s1", RunID: "r1",
			Provider: "deepseek", Model: "chat", InputTokens: 1, OutputTokens: 2,
			CostMicroUSD: 3, Source: "gateway", CreatedAt: now,
		}},
		{"UsageSum", store.UsageSum{InputTokens: 1, OutputTokens: 2, CostMicroUSD: 3, Calls: 4}},
		{"AuditLog", store.AuditLog{
			ID: "a1", TenantID: "t1", UserID: "u1", ActorEmail: "a@b.com",
			Action: "session.create", TargetType: "session", TargetID: "s1",
			Detail: json.RawMessage(`{}`), IP: "127.0.0.1", CreatedAt: now,
		}},
		{"Approval", runner.PendingApproval{
			RunID: "r1", ToolCallID: "tc1", Title: "执行命令", Kind: "execute",
			CreatedAt: now, ExpiresAt: now,
		}},
		{"Sandbox", sandboxView{
			ID: "s1", Provider: "local", State: "running", Workspace: "/w",
			PID: 42, CreatedAt: now, SessionID: "s1", SessionTitle: "标题",
			TenantID: "t1", UserID: "u1",
		}},
		{"FileEntry", fileEntry{
			Name: "a.md", Path: "docs/a.md", IsDir: false, Size: 12, ModTime: now,
		}},
		{"LifecyclePolicyView", policyView{
			SweepIntervalSeconds: 1, IdleTimeoutSeconds: 2, MaxAgeSeconds: 3,
			MaxPerTenant: 4, SweepInterval: "1s", IdleTimeout: "2s", MaxAge: "3s",
			Enabled: true,
		}},
		{"LifecycleReportView", reportView{
			Scanned: 1, Destroyed: 1, Orphaned: 1, Expired: 1, Idle: 1, Kept: 1,
			Errors: []string{"e"}, StartedAt: now, FinishedAt: now,
		}},
	}
}
