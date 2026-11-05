package obs

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
)

// New 构造结构化 logger。
//
// 为什么用 slog 而不是再引一个日志库：标准库自带，字段化输出、分级、
// 与 context 打通（InfoContext 会把 ctx 里的值交给 handler）。
//
// 为什么用 JSON 而不是 Text：这个服务的日志最终会被采集（容器 stdout → 日志平台），
// JSON 才能按字段检索；人眼看 JSON 的痛苦由本地 `jq` 缓解，比让生产日志无法过滤划算。
//
// level 接受 debug/info/warn/error（大小写不敏感，也接受 warning），
// 无法识别时退到 info 而不是 panic——启动参数写错不该让服务起不来。
func New(level string, w io.Writer) *slog.Logger {
	if w == nil {
		// nil writer 会让每次写日志都 panic，而这类路径通常只在高频/错误分支触发，
		// 等于「一出错就把进程打挂」。
		w = io.Discard
	}
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// staticSegments 是路由里的「固定词」，归一化时原样保留。
//
// 显式白名单而不是「猜哪些像 id」：新加一个路由忘了登记，最坏结果是这个路径
// 变成 :id（指标少一个维度），而不是把用户输入写进 label（基数爆炸）。
var staticSegments = map[string]struct{}{
	// 版本与通用
	"v1": {}, "v2": {}, "api": {}, "docs": {}, "static": {}, "assets": {},
	"healthz": {}, "readyz": {}, "metrics": {}, "openapi.json": {}, "meta": {},

	// 运行时资源
	"sessions": {}, "runs": {}, "files": {}, "file": {}, "events": {},
	"history": {}, "cancel": {}, "approvals": {},

	// 治理面
	"admin": {}, "tenants": {}, "users": {}, "apikeys": {}, "audit": {},
	"usage": {}, "billing": {}, "sandboxes": {}, "keys": {},

	// 认证
	"auth": {}, "login": {}, "logout": {}, "me": {}, "password": {},
	"bootstrap": {},

	// LLM 网关（OpenAI 兼容路径）
	"llm": {}, "chat": {}, "completions": {}, "llm-token": {}, "models": {},
}

// resourceSegments 的下一段一定是 id。白名单之外的兜底：
// 有些 id 形如 "abc123"、有些是 "u_9f2"、有些是纯字母的种子数据，
// 靠模式匹配总有漏网的，靠「父段是资源名」最稳。
var resourceSegments = map[string]struct{}{
	"sessions": {}, "runs": {}, "files": {}, "file": {}, "approvals": {},
	"sandboxes": {}, "tenants": {}, "users": {}, "apikeys": {}, "keys": {},
	"llm-token": {},
}

// NormalizePath 把真实路径归一成路由模板，用于指标 label 与日志。
//
// 例：/v1/sessions/abc123/runs → /v1/sessions/:id/runs
//
// 为什么必须做：路径里的 id 是用户数据，每个会话/run 都是一条新的时间序列。
// 不做归一化，一个正常使用的租户几天就能造出几十万条 label，Prometheus 直接 OOM。
// 日志同理——按路径聚合时，归一化之后才能回答「哪个接口慢」。
//
// 规则（按优先级）：
//  1. 固定词（staticSegments）原样保留；
//  2. {xxx} / :xxx 已经是模板，统一改写成 :id（Go 1.22 的 r.Pattern 是前者）；
//  3. 上一段是资源名（sessions/runs/...）→ :id；
//  4. 其余一律 :id：宁可少一个维度，也不要冒出无界 label。
//     这一条看着粗暴，但它是唯一能保证「基数只由路由表决定」的写法：
//     任何靠模式匹配（像不像 uuid / 多长 / 含不含数字）的方案，都会被
//     某个真实 id 形态绕过，绕过的那天就是 Prometheus 被拖垮的那天。
func NormalizePath(p string) string {
	if p == "" {
		return "/"
	}
	// 去掉 query / fragment 残留（调用方若传了完整 URL，这里也能兜住）。
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	// 末尾斜杠刻意不保留：/v1/sessions/ 与 /v1/sessions 是同一个路由，
	// 算成两个 label 会让面板上的 QPS 凭空减半。
	raw := strings.Split(p, "/")
	segs := make([]string, 0, len(raw))
	prev := ""
	for _, seg := range raw {
		if seg == "" {
			continue // 开头的空段、以及 // 造成的空段
		}
		segs = append(segs, normalizeSegment(seg, prev))
		prev = seg
	}

	return "/" + strings.Join(segs, "/")
}

func normalizeSegment(seg, prev string) string {
	if _, ok := staticSegments[strings.ToLower(seg)]; ok {
		return strings.ToLower(seg)
	}
	// Go 1.22 ServeMux 的模板：{id}、{toolCallId}
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return ":id"
	}
	if strings.HasPrefix(seg, ":") {
		return ":id"
	}
	if _, ok := resourceSegments[strings.ToLower(prev)]; ok {
		return ":id"
	}
	// 认不出来的段一律当变量。
	return ":id"
}

// HTTPMiddleware 打一条访问日志。
//
// 记什么、不记什么，值得说清楚：
//   - 记 path（已归一化）、method、status、耗时、租户、用户、来源 IP；
//   - 不记 query 与 body：它们可能有 prompt 正文、token、文件内容，
//     日志平台往往比业务库更容易被更多人看到。
//
// 耗时用毫秒整数：字符串里带单位（"1.2s"）在日志平台不好做聚合。
func HTTPMiddleware(lg *slog.Logger) func(http.Handler) http.Handler {
	if lg == nil {
		lg = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			attrs := []any{
				"method", r.Method,
				"path", routeLabel(r),
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				// tenant/user 在外层中间件里通常拿不到（认证发生在内层），
				// 拿不到就写 "-"，不要写空字符串——空字符串在聚合时会被当成「没有该字段」。
				"tenant", dashIfEmpty(tenantOf(r)),
				"user", dashIfEmpty(userOf(r)),
				"ip", dashIfEmpty(clientIP(r)),
			}

			// 状态码决定日志级别：5xx 是 error（要看），4xx 是 warn（可能被刷），
			// 其余 info。这样告警规则可以直接按 level 过滤，不必写正则解析 status。
			switch {
			case rec.status >= 500:
				lg.Error("http", attrs...)
			case rec.status >= 400:
				lg.Warn("http", attrs...)
			default:
				lg.Info("http", attrs...)
			}
		})
	}
}

func tenantOf(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.TenantID
	}
	return ""
}

func userOf(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.UserID
	}
	return ""
}

// clientIP 只信 X-Forwarded-For 的第一跳。
//
// 与 api.clientIP 同语义：放在这里是因为日志中间件在最外层，
// 拿不到 api 包内部的那个函数（且不能改 api.go 去导出它）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
