package egress

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
)

// unownedTenant 是「认不出归属」的流量挂的租户名。
//
// 为什么不丢进空租户：空串在 Log() 里表示「全部租户」，两者混在一起就再也
// 分不清「运营看全部」和「一条无主流量」。用固定名字还能让运营一键筛出来。
const unownedTenant = "unowned"

// owner 是一条流量背后的租户/用户/会话/run。
type owner struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
}

func (o owner) known() bool { return o.TenantID != "" }

// registry 是 sandboxID → 归属 的登记表，由 Controller.Register 填充。
//
// 代理本身看不到沙箱进程，只能从 CONNECT/请求里的 Proxy-Authorization 拿到
// sandboxID；沙箱生命周期归 runner 管，所以这里只存映射，不做回收判断——
// runner 该在沙箱销毁时调用 Unregister。
type registry struct {
	mu sync.RWMutex
	m  map[string]owner
}

func newRegistry() *registry {
	return &registry{m: make(map[string]owner)}
}

func (r *registry) register(sandboxID string, o owner) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return
	}
	r.mu.Lock()
	r.m[sandboxID] = o
	r.mu.Unlock()
}

func (r *registry) unregister(sandboxID string) {
	r.mu.Lock()
	delete(r.m, sandboxID)
	r.mu.Unlock()
}

func (r *registry) lookup(sandboxID string) (owner, bool) {
	r.mu.RLock()
	o, ok := r.m[sandboxID]
	r.mu.RUnlock()
	return o, ok
}

// sandboxFromProxyAuth 从 `Proxy-Authorization: Basic base64("<sandboxID>:<nonce>")`
// 里取出 sandboxID。nonce 目前只用来让每条沙箱的凭据不同（防止一个沙箱的
// header 被另一个复用后完全无差别），不参与校验——真正防伪要靠代理只监听
// 127.0.0.1 / 沙箱网络命名空间隔离，这一轮不做。
func sandboxFromProxyAuth(h string) string {
	const prefix = "basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		// 兼容不带 padding 的写法（有些客户端会自己裁）
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
		if err != nil {
			return ""
		}
	}
	user, _, ok := strings.Cut(string(raw), ":")
	if !ok {
		// 只有 sandboxID 没有 nonce 也接受：宁可认出归属，也不要因为格式
		// 小差异把所有流量打成 unowned（那会让审计失去意义）。
		user = string(raw)
	}
	return strings.TrimSpace(user)
}

// identify 把请求映射成归属；未登记返回零值（调用方标 unowned）。
func (p *proxy) identify(r *http.Request) owner {
	sid := sandboxFromProxyAuth(r.Header.Get("Proxy-Authorization"))
	if sid == "" {
		return owner{}
	}
	if o, ok := p.reg.lookup(sid); ok {
		return o
	}
	return owner{}
}
