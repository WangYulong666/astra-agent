// Package llm 是 LLM 网关：把模型调用收进控制面，由网关持有真 key 并记账。
//
// 为什么非要有这一层（设计文档 D4）：
//  1. 真 key 不进沙箱。沙箱里跑的是不可信代码，agent 执行一句 `env` 就能把
//     DEEPSEEK_API_KEY 读走并外传；网关在控制面持有 key，沙箱只拿 2h 虚拟 key。
//  2. 计费需要唯一可信来源。ACP 事件流里没有 token 用量，dsh 的 usage 只写在
//     自己沙箱内的私有缓存文件里，既不稳定也不属于我们的协议——只有网关
//     亲眼看到的 usage 才能作为账单依据。
//
// 职责边界：本包只做「转发 + 解析 usage + 回调」。落库、审计、额度这些
// 治理动作由调用方（internal/api）在回调里做，因为那需要请求身份与审计上下文。
package llm

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Provider 名字常量。目前只有一家是内置默认值，但结构上不假设只有一家。
const (
	ProviderDeepSeek = "deepseek"
)

// DefaultDeepSeekBaseURL 官方 OpenAI 兼容入口。
const DefaultDeepSeekBaseURL = "https://api.deepseek.com"

var (
	// ErrUnknownProvider 请求的 provider 名没有在配置里注册。
	ErrUnknownProvider = errors.New("llm: 未知 provider")
	// ErrNoAPIKey provider 注册了但没有 key。宁可在这里明确拒绝，
	// 也不要用空 key 去请求上游——那样只会拿回一个语义模糊的 401。
	ErrNoAPIKey = errors.New("llm: provider 未配置 API key")
)

// Provider 一个上游模型服务。
//
// APIKey 只存在于网关进程内存里，任何接口都不返回它（没有 json tag，
// 也不会被写进审计）。
type Provider struct {
	Name    string // "deepseek"
	BaseURL string // https://api.deepseek.com（不带尾部斜杠也会被兼容）
	APIKey  string
}

// Config 网关的静态配置。由启动参数翻译而来。
type Config struct {
	// Providers 可配置多个：map[名字]Provider。
	// 空 map 时仍然会内置 deepseek（没有 key，调用会明确报 ErrNoAPIKey）。
	Providers map[string]Provider
	// Default 默认 provider 名。空则取 deepseek；写了但没注册则回落到已注册的第一个（按名字排序，保证确定性）。
	Default string
}

// Registry provider 注册表。构造后只读，可被并发使用。
type Registry struct {
	providers map[string]Provider
	def       string
}

// NewRegistry 组装注册表，顺便补默认值。
func NewRegistry(cfg Config) *Registry {
	reg := &Registry{providers: make(map[string]Provider, len(cfg.Providers)+1)}
	for name, p := range cfg.Providers {
		n := strings.TrimSpace(p.Name)
		if n == "" {
			n = strings.TrimSpace(name)
		}
		if n == "" {
			continue
		}
		p.Name = n
		if strings.TrimSpace(p.BaseURL) == "" {
			p.BaseURL = defaultBaseURL(n)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		reg.providers[n] = p
	}

	// 内置 deepseek：让「只给一个 key」就能跑起来。没 key 时也能注册，
	// 错误留到真正调用时再报——启动期不因为没配 key 就直接崩。
	if _, ok := reg.providers[ProviderDeepSeek]; !ok {
		reg.providers[ProviderDeepSeek] = Provider{
			Name:    ProviderDeepSeek,
			BaseURL: DefaultDeepSeekBaseURL,
		}
	}

	reg.def = strings.TrimSpace(cfg.Default)
	if _, ok := reg.providers[reg.def]; !ok || reg.def == "" {
		if _, ok := reg.providers[ProviderDeepSeek]; ok {
			reg.def = ProviderDeepSeek
		} else {
			names := reg.Names()
			reg.def = names[0]
		}
	}
	return reg
}

// Names 已注册的 provider 名（排序后，便于日志与测试断言）。
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Default 默认 provider 名。
func (r *Registry) Default() string { return r.def }

// Lookup 按名字取 provider。空名字取默认。
func (r *Registry) Lookup(name string) (Provider, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = r.def
	}
	p, ok := r.providers[name]
	if !ok {
		return Provider{}, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return Provider{}, fmt.Errorf("%w: %q", ErrNoAPIKey, name)
	}
	return p, nil
}

// ProviderFor 按模型名推断 provider。
//
// 为什么用前缀而不是精确表：模型型号会随上游发版变（deepseek-chat → 某个日期后缀），
// 前缀匹配让我们不用每来一个型号就改一次配置。匹配不到就走默认。
func (r *Registry) ProviderFor(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m != "" {
		names := r.Names()
		// 长名字优先：避免 "deep" 这种短名抢走 "deepseek-chat"。
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		for _, n := range names {
			if strings.HasPrefix(m, strings.ToLower(n)) {
				return n
			}
		}
	}
	return r.def
}

// Resolve 结合「显式指定」与「按模型推断」两条线索选 provider。
func (r *Registry) Resolve(name, model string) (Provider, error) {
	if strings.TrimSpace(name) != "" {
		return r.Lookup(name)
	}
	return r.Lookup(r.ProviderFor(model))
}

func defaultBaseURL(name string) string {
	switch name {
	case ProviderDeepSeek:
		return DefaultDeepSeekBaseURL
	}
	return ""
}
