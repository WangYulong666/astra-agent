package llm

// Gateway 把 provider 注册表、价格表与转发器组装成「一次请求进来，转发 + 记账出去」。
//
// 为什么再多一层而不是让调用方直接用 Proxy：调用方（api 层）关心的是
// 「转发完了给我一个 (model, in, out) 的回调，我去落库和审计」，不应该关心
// 路径前缀、provider 解析、浅拷贝防竞态这些细节。

import (
	"context"
	"net/http"
	"strings"
)

// Gateway 一个进程一个：它持有真 key，多实例没有意义（也起不到隔离作用）。
type Gateway struct {
	// Proxy 转发器。Handler 内会浅拷贝一份挂 per-request 回调。
	Proxy *Proxy
	// Pricing 价格表。Cost 用它算钱，落库时的 CostMicroUSD 就该来自这里。
	Pricing *Pricing
	// OnUsage 进程级默认回调。api 层通常改为传 HandleWith 的 per-request 回调，
	// 因为审计需要请求身份与来源 IP。可以为 nil（只转发不记账，仅用于自测）。
	OnUsage func(meta Meta, model string, in, out int64)
}

// NewGateway 组装。pricing 为 nil 时用内置占位价（保证调用永远能算出钱，
// 而不是因为漏配就记 0 元）。
func NewGateway(reg *Registry, pricing *Pricing) *Gateway {
	if reg == nil {
		reg = NewRegistry(Config{})
	}
	if pricing == nil {
		pricing = NewPricing(nil)
	}
	return &Gateway{
		Proxy:   &Proxy{Registry: reg, Pricing: pricing},
		Pricing: pricing,
	}
}

// Handle 转发 + 用 Gateway.OnUsage 记账。
//
// 按设计不带返回值：它面向「记账回调已经装在网关上」的用法。
// api 层需要区分 400/502，所以那边用 ServeWith 拿 error——两条入口共用
// 同一份转发实现，不存在行为差异。
func (g *Gateway) Handle(ctx context.Context, w http.ResponseWriter, r *http.Request, meta Meta) {
	var cb func(model string, in, out int64)
	if g.OnUsage != nil {
		fn := g.OnUsage
		cb = func(model string, in, out int64) { fn(meta, model, in, out) }
	}
	_ = g.ServeWith(ctx, w, r, meta, cb)
}

// ServeWith 转发 + 用调用方给的 onUsage 记账，并把错误交回调用方。
//
// onUsage 里做的事（落库、审计）需要请求级上下文，所以它必须是 per-request 的。
// 为什么用浅拷贝而不是给共享 Proxy 赋回调：Proxy 被所有并发请求共用，
// 改它的字段就是数据竞争；拷贝一份再挂回调，字段本身仍然只读。
func (g *Gateway) ServeWith(ctx context.Context, w http.ResponseWriter, r *http.Request, meta Meta, onUsage func(model string, in, out int64)) error {
	if g == nil || g.Proxy == nil {
		return ErrUnknownProvider
	}
	p := *g.Proxy
	p.OnUsage = onUsage // nil 表示只转发不记账
	return p.Serve(ctx, w, r, meta)
}

// Cost 算一次调用的钱（微美元）。未知型号走兜底价，见 Pricing.Price。
func (g *Gateway) Cost(model string, in, out int64) int64 {
	if g == nil || g.Pricing == nil {
		return Cost(Price{}, in, out)
	}
	pr, _ := g.Pricing.Price(model)
	return Cost(pr, in, out)
}

// ProviderFor 按模型名给出 provider 名，用于落库的 Usage.Provider。
func (g *Gateway) ProviderFor(model string) string {
	if g == nil || g.Proxy == nil || g.Proxy.Registry == nil {
		return ""
	}
	return g.Proxy.Registry.ProviderFor(model)
}

// BasePath 给 agent 的 base_url（相对路径）。
//
// 用相对路径而不是绝对地址：沙箱里 agent 看到的 host 由平台注入，
// 写死地址会在反代、端口变化、多副本时全部失效。
func (g *Gateway) BasePath() string {
	if g == nil || g.Proxy == nil {
		return DefaultPathPrefix
	}
	if v := strings.TrimSpace(g.Proxy.PathPrefix); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultPathPrefix
}
