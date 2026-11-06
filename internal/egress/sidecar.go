package egress

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// sidecarTimeout 是一次转发的上限，避免慢上游把代理连接长期占住。
	sidecarTimeout = 20 * time.Second
	// maxSidecarBody 是回写给沙箱的响应体上限；超了按 502 拒绝，而不是截断
	// 一个 200（截断的响应会让客户端拿到看似成功却不完整的数据）。
	maxSidecarBody = 8 << 20 // 8MB
)

// handleSidecar 处理 /_connector/{name}/<path> 明文转发端点。
//
// HTTPS 走 CONNECT 时代理只搬密文，看不到也无法注入 Authorization（要 MITM，
// 本轮不做，见 ext.go/design 决策 3）。改由代理暴露一个本地明文端点：按 name
// 查 TokenFor 注入 header 后转发到连接器声明的 HTTPS 基址，token 一步都不进沙箱。
//
// SSRF 三道校验把「任意 name → 任意上游」堵死：
//  1. {name} 必须是配置过的连接器（BaseFor 认可）；
//  2. 基址必须是 https；
//  3. 基址 host 必须属于该连接器声明的 Hosts（NameFor 反查回同一个 name）。
//
// host 由控制面配置决定，沙箱只能给 {name} 与路径，不能指定目标主机。
func (p *proxy) handleSidecar(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	o := p.identify(r)

	rest := strings.TrimPrefix(r.URL.Path, sidecarPath)
	name, sub, _ := strings.Cut(rest, "/")
	if name == "" || name == "." || name == ".." {
		p.record(o, "", "", Deny, "bad-sidecar-target", started)
		http.Error(w, "egress: 非法的连接器名", http.StatusBadRequest)
		return
	}
	// 剩余路径里的 .. 会把请求打到基址以外的路径。host 已固定所以不是 SSRF，
	// 但仍要挡，避免借连接器白名单访问上游的意外端点。
	for _, seg := range strings.Split(sub, "/") {
		if seg == ".." {
			p.record(o, "", "", Deny, "bad-sidecar-target", started)
			http.Error(w, "egress: 非法的上游路径", http.StatusBadRequest)
			return
		}
	}

	base, ok := p.tokenProvider.BaseFor(name)
	if !ok {
		p.record(o, "", "", Deny, "bad-sidecar-target", started)
		http.Error(w, "egress: 未知连接器", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(base)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		p.record(o, "", "", Deny, "bad-sidecar-base", started)
		http.Error(w, "egress: 连接器基址必须是 https", http.StatusBadRequest)
		return
	}
	upstreamHost := u.Hostname()
	resolved, ok := p.tokenProvider.NameFor(upstreamHost)
	if !ok || resolved != name {
		p.record(o, upstreamHost, urlPort(u), Deny, "bad-sidecar-host", started)
		http.Error(w, "egress: 基址 host 不属于该连接器", http.StatusBadRequest)
		return
	}

	// 拼上游 URL：基址自带 path 作为前缀，接上 {name}/ 之后的部分，带上原 query。
	// 清掉 userinfo / fragment，避免基址里的 userinfo 变成发往上游的 Basic 头。
	up := *u
	up.User = nil
	up.Fragment = ""
	up.RawQuery = r.URL.RawQuery
	up.Path = joinSidecarPath(u.Path, sub)
	up.RawPath = ""
	target := up.String()

	ctx, cancel := context.WithTimeout(r.Context(), sidecarTimeout)
	defer cancel()

	header, value, ok, err := p.tokenProvider.TokenFor(ctx, o.TenantID, o.UserID, upstreamHost)
	if err != nil {
		p.record(o, upstreamHost, urlPort(u), Deny, "token-error", started)
		http.Error(w, "egress: 读取连接器凭据失败", http.StatusBadGateway)
		return
	}
	if !ok {
		// 没授权就别转发一个「没头」的请求：上游只会回 401，还把这次访问
		// 记成了合法出网。直接在这里挡掉更安全。
		p.record(o, upstreamHost, urlPort(u), Deny, "no-token", started)
		http.Error(w, "egress: 连接器未授权", http.StatusUnauthorized)
		return
	}
	if header == "" {
		header = "Authorization"
	}

	out, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		p.record(o, upstreamHost, urlPort(u), Deny, "bad-sidecar-target", started)
		http.Error(w, "egress: 无法构造上游请求", http.StatusBadRequest)
		return
	}
	out.Header = r.Header.Clone()
	removeHopByHop(out.Header)
	// 别把沙箱的代理身份泄漏给上游（Proxy-Authorization 已在 hopHeaders 里，
	// 这里再显式删一次，语义更清楚）。
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	out.Header.Set(header, value)
	out.Host = u.Host
	out.ContentLength = r.ContentLength

	resp, err := p.sidecarHTTP.RoundTrip(out)
	if err != nil {
		p.record(o, upstreamHost, urlPort(u), Deny, "upstream-error", started)
		http.Error(w, "egress: 上游不可达", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSidecarBody+1))
	if err != nil {
		p.record(o, upstreamHost, urlPort(u), Deny, "upstream-error", started)
		http.Error(w, "egress: 读取上游响应失败", http.StatusBadGateway)
		return
	}
	if len(body) > maxSidecarBody {
		p.record(o, upstreamHost, urlPort(u), Deny, "response-too-large", started)
		http.Error(w, "egress: 上游响应体过大", http.StatusBadGateway)
		return
	}

	entry := p.record(o, upstreamHost, urlPort(u), Allow, "sidecar:"+name, started)
	removeHopByHop(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	p.ring.update(entry, func(e *LogEntry) { e.BytesDown = int64(len(body)) })
}

// joinSidecarPath 把基址自带的 path 与 sidecar 剩余路径拼起来。
func joinSidecarPath(basePath, sub string) string {
	if sub == "" {
		return basePath
	}
	return strings.TrimSuffix(basePath, "/") + "/" + sub
}

// urlPort 取基址端口，缺省按 https 的 443。
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}
