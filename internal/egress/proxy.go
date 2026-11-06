package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// dialTimeout 是「连到目标」的上限。DNS + TCP 握手都算在里面；
	// 10 秒足够跨洋，又不至于让沙箱卡在一次黑洞路由上。
	dialTimeout = 10 * time.Second
	// tunnelIdleTimeout 是隧道的空闲上限。不设的话一条 CONNECT 能挂到
	// 进程退出；设了就能保证 Close() 之外还有一条自愈路径。
	tunnelIdleTimeout = 10 * time.Minute
	// headerTimeout 防止慢速 header 攻击把代理的连接数吃满。
	headerTimeout = 15 * time.Second
)

// counter 是并发安全的字节计数器（http.Transport 可能在自己的 goroutine
// 里读请求体，而主 goroutine 同时准备记录日志）。
type counter struct{ n atomic.Int64 }

func (c *counter) add(n int64) { c.n.Add(n) }
func (c *counter) load() int64 { return c.n.Load() }

type countingReadCloser struct {
	rc io.ReadCloser
	n  *counter
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.n.add(int64(n))
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

type countingWriter struct {
	w io.Writer
	n *counter
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.n.add(int64(n))
	}
	return n, err
}

// deadlineReader / deadlineConn 在每次读写前续一次 deadline，实现「空闲超时」
// 而不是「总时长超时」——长下载不该被一刀切，但彻底没动静的连接必须回收。
type deadlineReader struct {
	r io.Reader
	c net.Conn
	d time.Duration
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.d > 0 {
		_ = d.c.SetReadDeadline(time.Now().Add(d.d))
	}
	return d.r.Read(p)
}

type deadlineConn struct {
	net.Conn
	d time.Duration
}

func (d *deadlineConn) Write(p []byte) (int, error) {
	if d.d > 0 {
		_ = d.Conn.SetWriteDeadline(time.Now().Add(d.d))
	}
	return d.Conn.Write(p)
}

type closeWriter interface{ CloseWrite() error }

func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// proxy 同时实现 Controller 与 http.Handler。
type proxy struct {
	addr string
	srv  *http.Server

	ring *ring
	reg  *registry

	defaultPolicy Policy
	policyPath    string

	// policyMu 串行化「落盘 + 更新内存」；mu 只保护内存表的读写。
	policyMu sync.Mutex
	mu       sync.RWMutex
	policies map[string]Policy

	publicDialer *net.Dialer // 禁止私网
	anyDialer    *net.Dialer // 允许私网（策略显式开启时）
	publicHTTP   *http.Transport
	anyHTTP      *http.Transport

	closed    atomic.Bool
	closeOnce sync.Once

	trackMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// Open 启动出网代理。ListenAddr 为空按 ErrDisabled 返回——「没配=不启用」，
// 不要偷偷起一个默认端口的代理。
func Open(cfg Config) (Controller, error) {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return nil, ErrDisabled
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("egress: 监听 %s: %w", cfg.ListenAddr, err)
	}
	p, err := newProxy(cfg, ln)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return p, nil
}

func newProxy(cfg Config, ln net.Listener) (*proxy, error) {
	def, err := normalizePolicy(cfg.DefaultPolicy)
	if err != nil {
		return nil, err
	}
	dir := dataDir()
	path := ""
	if dir != "" {
		path = filepath.Join(dir, policyFileName)
	}
	p := &proxy{
		addr:          ln.Addr().String(),
		ring:          newRing(cfg.LogSize),
		reg:           newRegistry(),
		defaultPolicy: def,
		policyPath:    path,
		policies:      make(map[string]Policy),
		conns:         make(map[net.Conn]struct{}),
	}
	p.loadPolicies()

	p.publicDialer = &net.Dialer{Timeout: dialTimeout, Control: blockPrivate}
	p.anyDialer = &net.Dialer{Timeout: dialTimeout}
	p.publicHTTP = newTransport(p.publicDialer)
	p.anyHTTP = newTransport(p.anyDialer)

	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logf("代理已停止: %v", err)
		}
	}()
	return p, nil
}

// blockPrivate 在 DNS 解析之后、真正 connect 之前拿到 IP，挡住私网目标。
// 放在 Control 里而不是「先 LookupIP 再 Dial」：避免解析与拨号之间的
// DNS rebinding 把 127.0.0.1 换成公网 IP 或反过来。
func blockPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return errPrivateAddress
	}
	return nil
}

func newTransport(d *net.Dialer) *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // 自己就是代理，绝不能再往上走一层
		DialContext:           d.DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // 压缩是端到端语义，代理不该替客户端解压
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

func (p *proxy) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "egress: "+format+"\n", args...)
}

func (p *proxy) Enabled() bool { return !p.closed.Load() }

func (p *proxy) Addr() string { return p.addr }

func (p *proxy) Policy(_ context.Context, tenantID string) (Policy, error) {
	if tenantID == "" {
		return clonePolicy(p.defaultPolicy), nil
	}
	p.mu.RLock()
	pol, ok := p.policies[tenantID]
	p.mu.RUnlock()
	if !ok {
		return clonePolicy(p.defaultPolicy), nil
	}
	return clonePolicy(pol), nil
}

func (p *proxy) SetPolicy(_ context.Context, tenantID string, pol Policy) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("egress: tenantID 不能为空")
	}
	np, err := normalizePolicy(pol)
	if err != nil {
		return err
	}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()

	// 先落盘再改内存：写失败时不能让「重启后策略回滚」静默发生。
	snapshot := make(map[string]Policy, len(p.policies)+1)
	p.mu.RLock()
	for k, v := range p.policies {
		snapshot[k] = v
	}
	p.mu.RUnlock()
	snapshot[tenantID] = np
	if err := p.persist(snapshot); err != nil {
		return err
	}
	p.mu.Lock()
	p.policies[tenantID] = np
	p.mu.Unlock()
	return nil
}

func (p *proxy) Log(_ context.Context, tenantID string, limit int) ([]LogEntry, error) {
	return p.ring.list(tenantID, limit), nil
}

func (p *proxy) Register(sandboxID, tenantID, userID, sessionID, runID string) {
	p.reg.register(sandboxID, owner{
		TenantID:  tenantID,
		UserID:    userID,
		SessionID: sessionID,
		RunID:     runID,
	})
}

func (p *proxy) Unregister(sandboxID string) { p.reg.unregister(sandboxID) }

func (p *proxy) Close() error {
	var err error
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		if e := p.srv.Close(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			err = e
		}
		// http.Server.Close 不会碰 hijacked 连接（CONNECT 隧道全是），
		// 所以要自己把所有登记过的 conn 关掉，否则端口释放了隧道还活着。
		p.trackMu.Lock()
		for c := range p.conns {
			_ = c.Close()
		}
		p.conns = make(map[net.Conn]struct{})
		p.trackMu.Unlock()
		p.publicHTTP.CloseIdleConnections()
		p.anyHTTP.CloseIdleConnections()
	})
	return err
}

// track 登记一条需要被 Close() 兜底关闭的连接。
func (p *proxy) track(c net.Conn) func() {
	p.trackMu.Lock()
	if p.closed.Load() {
		p.trackMu.Unlock()
		_ = c.Close()
		return func() {}
	}
	p.conns[c] = struct{}{}
	p.trackMu.Unlock()
	return func() {
		p.trackMu.Lock()
		delete(p.conns, c)
		p.trackMu.Unlock()
	}
}

func (p *proxy) policyFor(tenantID string) Policy {
	if tenantID == "" {
		return p.defaultPolicy
	}
	p.mu.RLock()
	pol, ok := p.policies[tenantID]
	p.mu.RUnlock()
	if !ok {
		return p.defaultPolicy
	}
	return pol
}

func (p *proxy) dialerFor(allowPrivate bool) *net.Dialer {
	if allowPrivate {
		return p.anyDialer
	}
	return p.publicDialer
}

func (p *proxy) transportFor(allowPrivate bool) *http.Transport {
	if allowPrivate {
		return p.anyHTTP
	}
	return p.publicHTTP
}

// record 落一条审计。未登记归属的流量统一标 unowned（TenantID），决策本身
// 仍按默认策略走，不因为「不知道是谁」就放行或额外拒绝。Reason 只描述决策
// 依据，不混入归属信息——否则运营按原因筛日志时会多出一层前缀。
func (p *proxy) record(o owner, host, port string, dec Decision, reason string, started time.Time) *LogEntry {
	e := LogEntry{
		Time:      started,
		TenantID:  o.TenantID,
		UserID:    o.UserID,
		SessionID: o.SessionID,
		RunID:     o.RunID,
		Host:      normalizeHost(host),
		Port:      port,
		Decision:  dec,
		Reason:    reason,
	}
	if e.TenantID == "" {
		e.TenantID = unownedTenant
		if e.Reason == "" {
			e.Reason = "unowned"
		}
	}
	return p.ring.add(e)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// handleConnect 处理 HTTPS：客户端先 CONNECT，拿 200 后自己跑 TLS。
// 代理看不到明文，只能按 host:port 做策略，这正是「按域名管控」的边界。
func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	o := p.identify(r)

	target := r.Host
	if target == "" && r.URL != nil {
		target = r.URL.Host
	}
	host, port, err := splitTarget(target, "443")
	if err != nil {
		p.record(o, target, "", Deny, "bad-target", started)
		http.Error(w, "egress: 目标地址非法", http.StatusBadRequest)
		return
	}

	pol := p.policyFor(o.TenantID)
	dec, reason := decideHost(pol, host)
	if dec != Allow {
		p.record(o, host, port, dec, reason, started)
		http.Error(w, "egress: 出网被拒绝（"+reason+"）", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.dialerFor(pol.AllowPrivate).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	cancel()
	if err != nil {
		reason, status := "upstream-error", http.StatusBadGateway
		if errors.Is(err, errPrivateAddress) {
			reason, status = "private-address", http.StatusForbidden
		}
		p.record(o, host, port, Deny, reason, started)
		http.Error(w, "egress: 无法连接目标", status)
		return
	}
	defer upstream.Close()
	defer p.track(upstream)()

	hj, ok := w.(http.Hijacker)
	if !ok {
		p.record(o, host, port, Deny, "hijack-unsupported", started)
		http.Error(w, "egress: 当前服务器不支持连接劫持", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		p.record(o, host, port, Deny, "hijack-failed", started)
		return
	}
	defer client.Close()
	defer p.track(client)()

	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.record(o, host, port, Deny, "handshake-failed", started)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.record(o, host, port, Deny, "handshake-failed", started)
		return
	}

	entry := p.record(o, host, port, Allow, reason, started)
	up, down := tunnel(client, bufrw.Reader, upstream, tunnelIdleTimeout)
	p.ring.update(entry, func(e *LogEntry) {
		e.BytesUp = up
		e.BytesDown = down
	})
}

// tunnel 双向拷贝。返回值为「客户端→上游」「上游→客户端」的原始字节数，
// 对 HTTPS 而言就是 TLS 密文（含握手），不是明文体积。
func tunnel(client net.Conn, clientReader io.Reader, upstream net.Conn, idle time.Duration) (up, down int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		src := &deadlineReader{r: clientReader, c: client, d: idle}
		dst := &deadlineConn{Conn: upstream, d: idle}
		n, _ := io.Copy(dst, src)
		up = n
		halfClose(upstream)
	}()
	go func() {
		defer wg.Done()
		src := &deadlineReader{r: upstream, c: upstream, d: idle}
		dst := &deadlineConn{Conn: client, d: idle}
		n, _ := io.Copy(dst, src)
		down = n
		halfClose(client)
	}()
	wg.Wait()
	return up, down
}

// handleForward 处理普通 HTTP（absolute-URI）。http.Client 走代理时，
// http:// 目标走这条，https:// 目标走 CONNECT。
func (p *proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	o := p.identify(r)

	if r.URL == nil || !r.URL.IsAbs() || r.URL.Host == "" {
		p.record(o, "", "", Deny, "not-a-proxy-request", started)
		http.Error(w, "egress: 这是代理端口，请用 absolute-URI 或 CONNECT", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(r.URL.Scheme, "http") {
		p.record(o, r.URL.Host, "", Deny, "unsupported-scheme", started)
		http.Error(w, "egress: 只支持 http absolute-URI；https 请用 CONNECT", http.StatusBadRequest)
		return
	}

	host, port, err := splitTarget(r.URL.Host, "80")
	if err != nil {
		p.record(o, r.URL.Host, "", Deny, "bad-target", started)
		http.Error(w, "egress: 目标地址非法", http.StatusBadRequest)
		return
	}

	pol := p.policyFor(o.TenantID)
	dec, reason := decideHost(pol, host)
	if dec != Allow {
		p.record(o, host, port, dec, reason, started)
		http.Error(w, "egress: 出网被拒绝（"+reason+"）", http.StatusForbidden)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.URL.Host = r.URL.Host
	out.Header = r.Header.Clone()
	removeHopByHop(out.Header)
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	out.Close = false

	var up, down counter
	if r.Body != nil && r.Body != http.NoBody {
		out.Body = &countingReadCloser{rc: r.Body, n: &up}
	}

	resp, err := p.transportFor(pol.AllowPrivate).RoundTrip(out)
	if err != nil {
		reason, status := "upstream-error", http.StatusBadGateway
		if errors.Is(err, errPrivateAddress) {
			reason, status = "private-address", http.StatusForbidden
		}
		p.record(o, host, port, Deny, reason, started)
		http.Error(w, "egress: 上游不可达", status)
		return
	}
	defer resp.Body.Close()

	entry := p.record(o, host, port, Allow, reason, started)

	removeHopByHop(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(&countingWriter{w: w, n: &down}, resp.Body)

	// 回填字节：请求体可能还没被上游读完（比如上游提前写响应），
	// 但 RoundTrip 返回后已不再增长，读到的就是最终值。
	p.ring.update(entry, func(e *LogEntry) {
		e.BytesUp = up.load()
		e.BytesDown = down.load()
	})
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// hopHeaders 是逐跳头：只对「这一段连接」有意义，代理不能往下传。
// 不剥掉会出现「客户端要 close，结果代理把上游连接也带死」这类问题。
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHop(h http.Header) {
	for _, f := range h["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}
