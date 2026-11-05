// Package obs 是可观测性基础设施：指标、结构化日志、审计落库。
//
// 依赖方向是 obs → store（只读实体）与 obs → auth（只读身份），api 再依赖 obs。
// 反过来 obs 绝不 import api：api 的 Config 里放接口（api.Auditor），
// obs 用结构体去满足它——这样「没开可观测性」时 api 不必为 nil 逻辑拖进 prometheus。
package obs

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// run 终态。只统计这三种——queued / running / waiting_approval 是过程态，
// 把它们算进 astra_runs_total 会让「成功率」这个最常用的查询变成三段式减法。
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// 审批结论。
const (
	DecisionAllow   = "allow"
	DecisionDeny    = "deny"
	DecisionTimeout = "timeout"
)

// LLM token 方向。
const (
	DirectionInput  = "input"
	DirectionOutput = "output"
)

// 专用 registry，不用 prometheus.DefaultRegisterer。
//
// 理由：默认 registry 是全局单例，任何被 import 的库都能往上塞指标，
// 一旦两个包注册同名指标就是进程启动 panic；测试里也不方便反复重建。
// 独立的 registry 还能顺带把 /metrics 的暴露面收敛到「我们自己定义的 + 运行时」。
var registry = prometheus.NewRegistry()

var (
	// runsTotal 按终态计数。labels 里不放 session/run id：那是无界基数，
	// 一次压测就能把 Prometheus 打爆。
	runsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "astra_runs_total",
		Help: "run 终态计数，status=completed|failed|cancelled",
	}, []string{"status"})

	// runDuration 只观察终态 run 的耗时（StartedAt→EndedAt）。
	// 分桶覆盖「秒级问答」到「半小时长任务」，避免所有观测落进 +Inf。
	runDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "astra_run_duration_seconds",
		Help:    "run 耗时（秒），从 run 进入 running 到终态",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800},
	})

	// activeSessions 当前活跃会话数（有未结束 run，或最近仍在用）。
	activeSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "astra_active_sessions",
		Help: "当前活跃会话数",
	})

	// activeSandboxes 当前存活的沙箱数。它和会话数不一定相等：
	// 沙箱会在会话删除后残留，这个差值就是「该清理了」的信号。
	activeSandboxes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "astra_active_sandboxes",
		Help: "当前存活沙箱数",
	})

	// approvalsTotal 按结论计数。用 allow/deny/timeout 而不是「批准率」，
	// 是因为 timeout 单独看才有意义——它意味着有人把审批挂在屏幕上没管。
	approvalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "astra_approvals_total",
		Help: "审批结论计数，decision=allow|deny|timeout",
	}, []string{"decision"})

	// llmTokensTotal model 标签是有限的（deepseek/openai 的几个型号），
	// 可以在代码里做白名单收敛；这里不做，因为新增型号时白名单会成为漏统计的原因。
	llmTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "astra_llm_tokens_total",
		Help: "LLM token 用量，direction=input|output",
	}, []string{"model", "direction"})

	// llmCostMicroUSD 用微美元整数累加，和 store.Usage.CostMicroUSD 同一单位。
	// 用浮点美元会在百万次累加后漂移，对账时对不上。
	llmCostMicroUSDTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "astra_llm_cost_micro_usd_total",
		Help: "LLM 成本累计（微美元，1 美元 = 1e6）",
	}, []string{"model"})

	// httpRequestsTotal path 必须是「路由模板」而不是真实路径，
	// 否则 /v1/sessions/<每个会话 id> 每个 id 一条时间序列。
	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "astra_http_requests_total",
		Help: "HTTP 请求计数",
	}, []string{"method", "path", "status"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "astra_http_request_duration_seconds",
		Help:    "HTTP 请求耗时（秒）",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
)

func init() {
	registry.MustRegister(
		runsTotal,
		runDuration,
		activeSessions,
		activeSandboxes,
		approvalsTotal,
		llmTokensTotal,
		llmCostMicroUSDTotal,
		httpRequestsTotal,
		httpRequestDuration,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	// 预热已知标签组合：带 label 的指标在第一次 WithLabelValues 之前不会出现在
	// /metrics 输出里。终态/审批结论是有限枚举，先建出来，抓取端一启动就有曲线
	// （值 0 也有意义：它证明这条采集链路是通的）。
	for _, s := range []string{StatusCompleted, StatusFailed, StatusCancelled} {
		runsTotal.WithLabelValues(s)
	}
	for _, d := range []string{DecisionAllow, DecisionDeny, DecisionTimeout} {
		approvalsTotal.WithLabelValues(d)
	}

	enabled.Store(true)
}

// enabled 控制 /metrics 是否可用。
//
// 为什么用包级开关而不是 Config 字段：api.Config 目前没有指标字段，而这一期
// 不允许改 api.go。等主控把 Metrics 字段加进 Config 后，把这里换成配置即可，
// 语义不变（未配置指标 → 501，而不是给一个空 registry 让人误以为「没有流量」）。
//
// 用 atomic 而不是裸 bool：埋点函数会在 runner 的 goroutine 里并发调用，
// 而 SetEnabled 可能由 main 在装配期调用，裸 bool 就是一次 data race。
var enabled atomic.Bool

// SetEnabled 打开/关闭指标暴露。必须在任何请求之前调用（main 里装配时）。
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled 报告指标是否启用。
func Enabled() bool { return enabled.Load() }

// Handler 返回 /metrics 的 handler。
//
// 返回 nil 表示指标未启用，调用方应当回 501 并说明——这比返回一个只有
// go_* 运行时指标的空 registry 更诚实：后者会让人以为「业务没流量」。
func Handler() http.Handler {
	if !enabled.Load() {
		return nil
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// 采集出错的指标（比如重复注册）跳过并继续输出其余部分，
		// 不要让一次坏的注册让整页 500 —— 排障时最需要的就是这一页。
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// ── 记录入口 ─────────────────────────────────────────────────
//
// 下面这些函数是给 runner / 网关 / HTTP 层调用的埋点入口。
// 都做了 nil 保护是没必要的（包级变量永不为 nil），所以只有一个职责：别让调用方
// 为了写指标而 import prometheus。

// RecordRunFinished 记一次 run 终态。
//
// 未知 status（比如将来新增的 interrupted）统一记到 failed：
// 宁可让成功率看起来偏低，也不要凭空多一条没有告警规则覆盖的时间序列。
func RecordRunFinished(status string, d time.Duration) {
	if !enabled.Load() {
		return
	}
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled:
	default:
		status = StatusFailed
	}
	runsTotal.WithLabelValues(status).Inc()
	// 负耗时来自时钟回拨；超过 24h 的几乎只可能来自「StartedAt 是零值」
	// （run 还没进 running 就失败/取消了，EndedAt 减零值等于从公元 1 年算起）。
	// 两种都不观测：一条几十万秒的样本会把 histogram 的 +Inf 桶和中位数一起带偏。
	if d >= 0 && d < 24*time.Hour {
		runDuration.Observe(d.Seconds())
	}
}

// SetActiveSessions 设置活跃会话数。
func SetActiveSessions(n int) {
	if !enabled.Load() {
		return
	}
	activeSessions.Set(float64(n))
}

// SetActiveSandboxes 设置存活沙箱数。
func SetActiveSandboxes(n int) {
	if !enabled.Load() {
		return
	}
	activeSandboxes.Set(float64(n))
}

// RecordApproval 记一次审批结论。
func RecordApproval(decision string) {
	if !enabled.Load() {
		return
	}
	switch decision {
	case DecisionAllow, DecisionDeny, DecisionTimeout:
	default:
		// 未知结论不落标签，避免任意字符串撑爆基数；同时留一条 deny 让总量守恒。
		decision = DecisionDeny
	}
	approvalsTotal.WithLabelValues(decision).Inc()
}

// RecordLLMUsage 记一次模型调用的 token 与成本。
func RecordLLMUsage(model string, inputTokens, outputTokens, costMicroUSD int64) {
	if !enabled.Load() {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if inputTokens > 0 {
		llmTokensTotal.WithLabelValues(model, DirectionInput).Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		llmTokensTotal.WithLabelValues(model, DirectionOutput).Add(float64(outputTokens))
	}
	if costMicroUSD > 0 {
		llmCostMicroUSDTotal.WithLabelValues(model).Add(float64(costMicroUSD))
	}
}

// ── HTTP 指标中间件 ──────────────────────────────────────────

// MetricsMiddleware 统计 HTTP 请求数与耗时。
//
// 必须包在最外层（api.Handler() 外面）：这样 404、CORS 预检、panic 兜底这些
// 也会被计入，否则「请求少了」的时候分不清是没人调还是路由没匹配上。
func MetricsMiddleware(next http.Handler) http.Handler {
	if !enabled.Load() || next == nil {
		// nil 时返回一个空 handler 而不是 nil：调用方可能直接把它赋给
		// http.Server.Handler，赋 nil 会让整个服务用默认 mux（路由全丢）。
		if next == nil {
			return http.NotFoundHandler()
		}
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		path := routeLabel(r)

		httpRequestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
	})
}

// routeLabel 取「低基数」的路径标签。
//
// 优先用 mux 匹配到的路由模板（Go 1.22+ 的 r.Pattern）：它就是路由表本身，
// 天然无基数风险，也不需要维护静态词白名单。注意它的格式是
// "GET /v1/sessions/{id}"——带方法前缀，必须剥掉，否则拼出来的 label 会是
// "/:id/v1/sessions/:id" 这种把方法当路径段的怪东西。
//
// 拿不到 pattern（404、没经过 ServeMux、测试直连）时退回手写归一化。
func routeLabel(r *http.Request) string {
	p := r.Pattern
	if p == "" {
		return NormalizePath(r.URL.Path)
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = p[i+1:] // "GET /v1/..." → "/v1/..."
	}
	return NormalizePath(p)
}

// statusRecorder 捕获状态码。
//
// 必须转发 Flush / Unwrap：SSE 的 handler 会断言 w.(http.Flusher)，
// http.NewResponseController 也会沿 Unwrap 找底层连接。
// 不实现的话，包一层指标就把事件流悄悄改成了「攒完再发」——最难查的那种回归。
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	// 直接 Write 而不显式 WriteHeader 时，net/http 隐式回 200——这里补上，
	// 否则 rec.status 会停在初始值，把「没写头」和「显式 200」混为一谈。
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
