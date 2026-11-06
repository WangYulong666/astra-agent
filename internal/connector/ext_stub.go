package connector

// 本文件原是 v3 装配桩（ext_stub.go），现为「连接器 proxy 注入」的真实现：
// TokenFor（短期缓存 + 单飞合并回源）、ConnectorForHost（自建 host 匹配）。
// 保持 connector 零内部依赖：host 匹配规则自己实现，语义与 egress.matchPattern
// 一致，但不 import egress。

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// tokenCacheMaxTTL 是不过期/长有效期 token 的缓存上限：即便凭据本身有效期
	// 很长也定期回源，缩短 Delete/PutKey 到「sidecar 停止使用旧 token」的窗口
	//（写操作还会主动失效缓存）。
	tokenCacheMaxTTL = 5 * time.Minute
	// tokenCacheMinTTL 兜底：token 已接近/超过有效期时仍给一个极短缓存，避免
	// 「过期但不可刷新」的凭据在每个请求上重复解密 vault。
	tokenCacheMinTTL = 2 * time.Second
)

// tokenCacheEntry 是 TokenFor 的缓存条目（manager 的 cacheMu/tokenCache 见 manager.go）。
// inflight != nil 表示正有 goroutine 在回源（可能触发刷新写库），其它并发请求等
// 这个 channel 关闭后共享它的结果，保证同一 key 同时只刷新一次。
type tokenCacheEntry struct {
	token      string
	validUntil time.Time
	inflight   chan struct{}
	err        error // 仅回源结果：失败时记在这里，等待者共享同一个错误
}

func tokenCacheKey(tenantID, userID, name string) string {
	return tenantID + "\x00" + userID + "\x00" + name
}

// cacheValidUntil 决定这条 token 缓存到什么时候：
//   - 有 Expires：缓存到 Expires - refreshSkew（与 credential 的刷新判断一致，
//     缓存不会比 token 的「逻辑过期」活得更久），再截到 now+tokenCacheMaxTTL；
//   - 无 Expires（apikey / 提供方没给过期时间）：缓存 tokenCacheMaxTTL。
//
// 两种情况都至少给 tokenCacheMinTTL，避免过期凭据把高频请求全打回源。
func cacheValidUntil(now time.Time, expires time.Time) time.Time {
	until := now.Add(tokenCacheMaxTTL)
	if !expires.IsZero() {
		if boundary := expires.Add(-refreshSkew); boundary.Before(until) {
			until = boundary
		}
	}
	if floor := now.Add(tokenCacheMinTTL); until.Before(floor) {
		until = floor
	}
	return until
}

// invalidateTokenCache 在「写库改变 token」后让缓存失效。正在回源（inflight）的
// 条目不动：回源结束会写入刷新后的新值，删掉反而会让并发请求重复刷新。
func (m *manager) invalidateTokenCache(tenantID, userID, name string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	key := tokenCacheKey(tenantID, userID, name)
	if e, ok := m.tokenCache[key]; ok && e.inflight == nil {
		delete(m.tokenCache, key)
	}
}

// TokenFor 取（必要时刷新）某用户对某连接器的明文 token，只给控制面用。
//
// 复用 m.credential（已含 refreshSkew + refresh 刷新）。高频路径加短期缓存：
// 命中且未到 validUntil 直接返回，不解密 vault、不刷新；未命中用「单飞」合并同
// key 的并发回源，避免并发重复刷新落盘、重复打 token 端点（refresh token 轮换时
// 尤其危险）。写操作（PutKey/Delete/Exchange/refresh）会失效缓存。
func (m *manager) TokenFor(ctx context.Context, tenantID, userID, name string) (string, error) {
	if !m.Enabled() {
		return "", ErrDisabled
	}
	key := tokenCacheKey(tenantID, userID, name)
	now := time.Now()

	m.cacheMu.Lock()
	if m.tokenCache == nil {
		m.tokenCache = make(map[string]tokenCacheEntry)
	}
	e := m.tokenCache[key]
	switch {
	case e.inflight != nil:
		ch := e.inflight
		m.cacheMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		// 回源者已写好结果（含错误）；共享它，不再各自重试。
		m.cacheMu.Lock()
		res := m.tokenCache[key]
		m.cacheMu.Unlock()
		if res.err != nil {
			return "", res.err
		}
		if res.token != "" {
			return res.token, nil
		}
		return "", fmt.Errorf("%w: %s 回源未取到 token", ErrNotConnected, name)
	case e.token != "" && now.Before(e.validUntil):
		m.cacheMu.Unlock()
		return e.token, nil
	default:
		// 成为该 key 的唯一回源者。
		ch := make(chan struct{})
		m.tokenCache[key] = tokenCacheEntry{inflight: ch}
		m.cacheMu.Unlock()
		return m.loadToken(ctx, key, ch, tenantID, userID, name)
	}
}

// loadToken 回源取 credential、写入缓存并唤醒等待者。失败不缓存 token（写一条带
// err 的占位），下次调用会重新回源重试；等待者共享这次的错误，不放大成惊群。
func (m *manager) loadToken(ctx context.Context, key string, ch chan struct{}, tenantID, userID, name string) (string, error) {
	cred, err := m.credential(ctx, tenantID, userID, name)
	var token string
	if err == nil {
		if cred.Token == "" {
			// 没授权 / 凭据异常：绝不返回「空串 + nil」。
			err = fmt.Errorf("%w: %s 的 token 为空，请重新连接", ErrNotConnected, name)
		} else {
			token = cred.Token
		}
	}

	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if err != nil {
		m.tokenCache[key] = tokenCacheEntry{err: err}
	} else {
		m.tokenCache[key] = tokenCacheEntry{
			token:      token,
			validUntil: cacheValidUntil(time.Now(), cred.Expires),
		}
	}
	close(ch)
	return token, err
}

// normalizeHost 归一对端可能写出来的主机名：去空白、转小写、去末尾根点、去端口
// （也顺带处理 IPv6 方括号）。端口不参与匹配，与 egress.normalizeHost 同语义。
func normalizeHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// matchPattern 判断 host 是否命中 pattern，规则与 egress.matchPattern 一致：
//
//   - 全部
//     *.example.com    子域（含 foo.example.com，不含裸域 example.com）
//     *example.com     后缀匹配（含 example.com 本身）
//     api.example.com  精确匹配
//
// 全部大小写不敏感，端口在两边都会被剥掉。
func matchPattern(pattern, host string) bool {
	pat := normalizeHost(pattern)
	h := normalizeHost(host)
	if pat == "" || h == "" {
		return false
	}
	switch {
	case pat == "*":
		return true
	case strings.HasPrefix(pat, "*."):
		suffix := pat[1:] // ".example.com"
		return strings.HasSuffix(h, suffix) && len(h) > len(suffix)
	case strings.HasPrefix(pat, "*"):
		return strings.HasSuffix(h, pat[1:])
	default:
		return h == pat
	}
}

// ConnectorForHost 按目标域名反查连接器定义：去端口、大小写不敏感；精确匹配优先
// 于 *. 通配（与 egress 策略匹配优先级一致）。没有 Hosts 的连接器不参与反查。
func (m *manager) ConnectorForHost(host string) (Connector, bool) {
	return matchDefinitions(host, definitions)
}

// matchDefinitions 在给定定义集里按「精确优先、其次通配」反查。拆成纯函数是为了
// 能被单测用自定义定义验证优先级，而不必改包级 definitions。
func matchDefinitions(host string, defs []definition) (Connector, bool) {
	h := normalizeHost(host)
	if h == "" {
		return Connector{}, false
	}
	for _, wantWildcard := range []bool{false, true} {
		for _, d := range defs {
			for _, p := range d.Hosts {
				if strings.Contains(p, "*") == wantWildcard && matchPattern(p, h) {
					return cloneConnector(d.Connector), true
				}
			}
		}
	}
	return Connector{}, false
}
