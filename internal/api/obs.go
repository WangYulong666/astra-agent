package api

// 可观测性挂载点：/metrics 与 /readyz。
//
// 归属：T4 实现。指标定义在 internal/obs，这里只做 HTTP 出口——不在这里定义
// 任何 prometheus 对象，是为了让「有哪些指标」只在一个文件里可查。

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/WangYulong666/astra-agent/internal/obs"
)

// WrapMetrics 在 api.Handler() 的最外层统计 HTTP 指标。
//
// 用法（主控侧一行）：
//
//	h, _ := api.Handler(cfg)
//	srv.Handler = api.WrapMetrics(h)
//
// 为什么必须是「最外层」：里面已经包了 withRecover(withCORS(mux))。包在中间的话，
// CORS 预检的 204 和 panic 兜底的 500 都不会进指标——而排查事故时最先想知道
// 的恰恰是「5xx 涨了没有」。
//
// 为什么这个函数放在这里而不是 api.go：api.go 是主控的文件，本期不改。
// 语义上它属于「装配」，放在 obs.go 与 /metrics 出口同一文件里反而更好找。
func WrapMetrics(next http.Handler) http.Handler {
	return obs.MetricsMiddleware(next)
}

// metricsHandler 输出 Prometheus 文本格式。
//
// 未启用指标时回 501 而不是 200 + 空 body：空 registry 的 /metrics 会让人以为
// 「服务没流量」，而 501 会直接告诉抓取端「这里没接指标」。
func (h *handler) metricsHandler(w http.ResponseWriter, r *http.Request) {
	hd := obs.Handler()
	if hd == nil {
		fail(w, http.StatusNotImplemented, "指标未启用：obs.SetEnabled(false)，或装配时未传入指标")
		return
	}
	hd.ServeHTTP(w, r)
}

// readyz 检查依赖是否可用。
//
// 与 /healthz 的分工必须清楚，否则这个接口没有存在的意义：
//   - /healthz 只回答「进程还活着吗」——它永远 200，用于存活探针（liveness）。
//     进程卡在死锁里时它也返回 200，这不是 bug，是 liveness 的定义。
//   - /readyz 回答「现在能不能接流量」——它真的去戳依赖，不可用就 503，
//     用于就绪探针（readiness），K8s 会据此把实例摘出负载均衡。
//
// 所以这里不「只返回 ok」：那样两个接口就完全一样，而流量会被打进一个
// 连数据库都连不上的实例。
//
// 探活手段选 ListSessions（1s 超时）：
//   - 它走的是「读」路径，不会污染数据；
//   - file store 与 SQL store 都实现了它，不依赖治理层是否启用；
//   - 只取 1s：探针本身不能成为拖慢 shutdown 的原因，慢库应该表现为 503。
//
// 刻意不在这里检查 sandbox provider：List 可能要访问远端 runtime，
// 一个正忙的 provider 会让整个实例被摘出流量，而实际上已有连接还能用。
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	// 整体 1s 预算：两个依赖共用同一个 deadline，
	// 否则串行检查最坏要等 2s，超过 K8s 默认的 timeoutSeconds(1)。
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	type check struct {
		name string
		err  error
	}
	var checks []check

	// Store 是硬依赖：会话、run、事件全在里面，它挂了什么都做不了。
	if h.cfg.Store == nil {
		checks = append(checks, check{"store", errors.New("store 未装配")})
	} else if _, err := h.cfg.Store.ListSessions(ctx); err != nil {
		checks = append(checks, check{"store", err})
	} else {
		checks = append(checks, check{"store", nil})
	}

	// Admin 是可选依赖（file store 模式没有治理层）。装配了就必须可用：
	// 否则登录、审计、用量全静默失败，表现为「用户能进但什么都记不下来」。
	if h.cfg.Admin != nil {
		if _, err := h.cfg.Admin.ListTenants(ctx); err != nil {
			checks = append(checks, check{"admin", err})
		} else {
			checks = append(checks, check{"admin", nil})
		}
	}

	// 一旦第一个依赖超时，后面那个必然也超时（共用一个 deadline），
	// 直接把仍失败的都列出来，比让运维一个个试要省事。
	depStatus := make(map[string]string, len(checks))
	var failed []string
	for _, c := range checks {
		if c.err != nil {
			depStatus[c.name] = c.err.Error()
			failed = append(failed, c.name)
			continue
		}
		depStatus[c.name] = "ok"
	}

	if len(failed) > 0 {
		// 503 而不是 500：这不是「代码错了」，是「依赖没准备好」，
		// 负载均衡按这个语义摘流量、等它恢复。
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"failed": failed, // 明确说是哪个依赖挂了
			"checks": depStatus,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"checks": depStatus,
	})
}
