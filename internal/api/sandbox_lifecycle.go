package api

// 沙箱生命周期治理的管理接口。
//
// 与 sandbox_usage.go 的分界：那个文件回答「现在有哪些沙箱」，这个文件回答
// 「回收器在按什么规则动手、上一轮动了哪些、以及现在立刻动一次」。
//
// 路由现状（platform_routes.go，归属主控）：/v1/admin/lifecycle 尚未注册。
// 因此本文件的 handler 写成「接收 Reaper、返回 http.HandlerFunc」的形式，
// 谁注册谁负责套认证——这样文件本身不依赖 api.Config 的字段，
// 主控加字段时不会连带改动这里。注册码见文件末尾注释。

import (
	"net/http"
	"time"

	"github.com/WangYulong666/astra-agent/internal/lifecycle"
)

// policyView 是对外的策略快照。
//
// 时长同时给秒数与人类可读串：前端要画「30 分钟」这类文案，运维看 API 时要能直接读；
// 只给纳秒会让两拨人都要自己换算一次。
type policyView struct {
	SweepIntervalSeconds int64  `json:"sweepIntervalSeconds"`
	IdleTimeoutSeconds   int64  `json:"idleTimeoutSeconds"`
	MaxAgeSeconds        int64  `json:"maxAgeSeconds"`
	MaxPerTenant         int    `json:"maxPerTenant"`
	SweepInterval        string `json:"sweepInterval"`
	IdleTimeout          string `json:"idleTimeout"`
	MaxAge               string `json:"maxAge"`
	// Enabled 回收是否真的会动手。全零策略下后台循环不启动、Sweep 也不销毁任何东西，
	// 前端据此把「立即清理」按钮置灰，比让人点一下拿到全 0 报告更诚实。
	Enabled bool `json:"enabled"`
}

type reportView struct {
	Scanned    int       `json:"scanned"`
	Destroyed  int       `json:"destroyed"`
	Orphaned   int       `json:"orphaned"`
	Expired    int       `json:"expired"`
	Idle       int       `json:"idle"`
	Kept       int       `json:"kept"`
	Errors     []string  `json:"errors"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

type lifecycleStatus struct {
	Policy policyView `json:"policy"`
	// LastSweep 为 null 表示进程起来后还没扫过（比如只跑手动模式且没人点过）。
	// 区分「null」和「全 0 报告」很重要：后者是「扫了，什么都没找到」。
	LastSweep *reportView `json:"lastSweep"`
}

func newPolicyView(p lifecycle.Policy, enabled bool) policyView {
	return policyView{
		SweepIntervalSeconds: int64(p.SweepInterval / time.Second),
		IdleTimeoutSeconds:   int64(p.IdleTimeout / time.Second),
		MaxAgeSeconds:        int64(p.MaxAge / time.Second),
		MaxPerTenant:         p.MaxPerTenant,
		SweepInterval:        durText(p.SweepInterval),
		IdleTimeout:          durText(p.IdleTimeout),
		MaxAge:               durText(p.MaxAge),
		Enabled:              enabled,
	}
}

func newReportView(r lifecycle.Report) reportView {
	errs := r.Errors
	if errs == nil {
		// 回 [] 而不是 null：前端不必为「没有错误」多写一个判空分支。
		errs = []string{}
	}
	return reportView{
		Scanned:    r.Scanned,
		Destroyed:  r.Destroyed,
		Orphaned:   r.Orphaned,
		Expired:    r.Expired,
		Idle:       r.Idle,
		Kept:       r.Kept,
		Errors:     errs,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
	}
}

// durText 把时长写成可读串。0 显示 off 而不是 "0s"，避免被误读成「立刻超时」。
func durText(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

// LifecycleStatus 返回 GET /v1/admin/lifecycle 的 handler：当前策略 + 上一次报告。
//
// 为什么把「策略」也放进来：运维看到「沙箱没被回收」时，第一件事是确认
// 回收到底有没有开、阈值是多少。分成两个接口会让人永远少看一半信息。
func LifecycleStatus(rp *lifecycle.Reaper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rp == nil {
			fail(w, http.StatusNotImplemented, "沙箱回收未启用")
			return
		}
		out := lifecycleStatus{Policy: newPolicyView(rp.Policy(), rp.Enabled())}
		if rep, ok := rp.LastReport(); ok {
			v := newReportView(rep)
			out.LastSweep = &v
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// LifecycleSweep 返回 POST /v1/admin/lifecycle/sweep 的 handler：同步跑一轮。
//
// 同步而不是异步：手动清理是低频操作，调用者（管理后台的按钮）需要知道
// 「这次到底清掉了什么」。异步只会多出一个「查询任务状态」的接口。
// Reaper 内部有互斥，与后台循环撞上时会排队，不会重复销毁。
func LifecycleSweep(rp *lifecycle.Reaper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rp == nil {
			fail(w, http.StatusNotImplemented, "沙箱回收未启用")
			return
		}
		rep, err := rp.Sweep(r.Context())
		if err != nil {
			// 整轮无法进行（枚举失败）：500，并且把已经拿到的部分计数一起回，
			// 便于判断是「一个都没扫到」还是「扫到一半挂了」。
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  err.Error(),
				"report": newReportView(rep),
			})
			return
		}
		writeJSON(w, http.StatusOK, newReportView(rep))
	}
}

// 注册码（由主控加到 internal/api/platform_routes.go 的 registerPlatform 里，
// 与沙箱管理面同段；两者都是「管沙箱」的操作，权限用同一个 Action）：
//
//	mux.HandleFunc("GET /v1/admin/lifecycle", h.authed(LifecycleStatus(h.cfg.Reaper), auth.ActionSandboxManage))
//	mux.HandleFunc("POST /v1/admin/lifecycle/sweep", h.authed(LifecycleSweep(h.cfg.Reaper), auth.ActionSandboxManage))
//
// 依赖 api.Config 上新增一个字段（api.go 由主控维护）：
//
//	// Reaper 沙箱回收器。nil 表示治理未启用，两个管理端点回 501。
//	Reaper *lifecycle.Reaper
