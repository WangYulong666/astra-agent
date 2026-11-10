package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/xtaci/smux"
)

const (
	defaultMaxStreams  = 128
	defaultIdleTimeout = 90 * time.Second

	// upstreamScheme/Host 只是给 http.Transport 一个合法的 URL 骨架，
	// 真正的连接由 deviceSession.dial 换成 smux 流，所以值本身无意义。
	upstreamScheme = "http"
	upstreamHost   = "local"
)

// openServerImpl 启动 relay。
func openServerImpl(cfg ServerConfig) (Server, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("relay: Addr 不能为空")
	}

	lg := cfg.Logger
	if lg == nil {
		lg = log.New(os.Stderr, "relay ", log.LstdFlags)
	}

	maxStreams := cfg.MaxStreams
	if maxStreams <= 0 {
		maxStreams = defaultMaxStreams
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	if cfg.Token == "" {
		lg.Printf("⚠ 调试模式：relay 未配置 Token，只允许 127.0.0.1 连入（任何人都能注册设备 id 的部署形态不可接受）")
	}

	var ln net.Listener
	var err error
	if cfg.TLS != nil {
		ln, err = tls.Listen("tcp", cfg.Addr, cfg.TLS)
	} else {
		ln, err = net.Listen("tcp", cfg.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("relay: 监听 %s 失败: %w", cfg.Addr, err)
	}

	s := &relayServer{
		cfg:        cfg,
		lg:         lg,
		ln:         ln,
		devices:    make(map[string]*deviceSession),
		maxStreams: int64(maxStreams),
		idle:       idle,
		done:       make(chan struct{}),
	}
	s.hs = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 隧道是长连接，任何全局读写超时都会误杀，这里全部交给 smux 的 keepalive。
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}

	go func() {
		err := s.hs.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if !s.isClosed() {
				lg.Printf("relay: 服务退出: %v", err)
			}
		}
	}()

	scheme := "http"
	if cfg.TLS != nil {
		scheme = "https"
	}
	lg.Printf("relay 已启动：监听 %s（%s，调试模式=%v，公网 URL=%s，并发流上限 %d，空闲超时 %s）",
		s.Addr(), scheme, cfg.Token == "", orDash(cfg.PublicURL), maxStreams, idle)
	return s, nil
}

type relayServer struct {
	cfg ServerConfig
	lg  *log.Logger
	ln  net.Listener
	hs  *http.Server

	mu      sync.RWMutex
	devices map[string]*deviceSession

	maxStreams int64
	idle       time.Duration

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	wg sync.WaitGroup // 会话监听协程 + 设备连接协程
}

// deviceSession 是「一台在线设备」：一条 WSS + 一个 smux session + 专属反向代理。
// 每个设备一套 Transport，是因为 DialContext 必须知道「这条流开到哪台设备」——
// 挂在 Transport 上比在请求 context 里传设备 id 少一层约定。
type deviceSession struct {
	id   string
	sess *smux.Session
	ws   *websocket.Conn
	// conn 是「底层 WSS 死掉」的探测点，见 conn.go。
	conn *watchedConn
	// prefix 是浏览器侧前缀，形如 /t/dev-1；Director 用它剥路径。
	prefix string
	proxy  *httputil.ReverseProxy

	inflight atomic.Int64

	closeOnce sync.Once
}

func (s *relayServer) Addr() string { return s.ln.Addr().String() }

func (s *relayServer) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Close 可重复调用。顺序很重要：先摘设备（让 /relay/tunnel 的处理协程返回），
// 再 Shutdown HTTP——反过来会一直等到空闲超时才退出。
func (s *relayServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)

		s.mu.Lock()
		victims := make([]*deviceSession, 0, len(s.devices))
		for id, ds := range s.devices {
			victims = append(victims, ds)
			delete(s.devices, id)
		}
		s.mu.Unlock()
		for _, ds := range victims {
			ds.close()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.hs.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.closeErr = err
		}
		// Shutdown 关掉 listener 后 Serve 返回，但已经 hijack 的连接不受其管辖，
		// 这里兜底再关一次（smux 会话关闭后 ws 也会关）。
		for _, ds := range victims {
			ds.close()
		}
		_ = s.ln.Close()
		s.wg.Wait()
		s.lg.Printf("relay 已关闭（监听 %s）", s.ln.Addr().String())
	})
	return s.closeErr
}

func (s *relayServer) Online() []string {
	s.mu.RLock()
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

func (s *relayServer) lookup(id string) *deviceSession {
	s.mu.RLock()
	ds := s.devices[id]
	s.mu.RUnlock()
	return ds
}

func (s *relayServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_relay/healthz", s.handleHealthz)
	mux.HandleFunc("/relay/tunnel", s.handleTunnel)
	mux.HandleFunc("/t/", s.handleProxy)
	return mux
}

func (s *relayServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	s.mu.RLock()
	n := len(s.devices)
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "devices": n})
}

// handleTunnel 是设备端入口：校验票据 → 升级 WS → 起 smux 服务端 → 登记。
// 处理协程会一直阻塞到会话结束，因此隧道断开时这里立刻感知。
func (s *relayServer) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET"})
		return
	}
	q := r.URL.Query()
	device := q.Get("device")
	ts := q.Get("ts")
	sig := q.Get("sig")

	if !deviceIDPattern.MatchString(device) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "device 非法"})
		return
	}

	if s.cfg.Token == "" {
		// 调试模式没有共享密钥可用，唯一的边界就是「本机」。
		if !isLoopback(r.RemoteAddr) {
			s.lg.Printf("relay: 拒绝非回环设备注册（调试模式无 token）来自 %s", r.RemoteAddr)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "调试模式只接受 127.0.0.1 连接"})
			return
		}
	} else if err := verifyTicket(s.cfg.Token, device, ts, sig, time.Now()); err != nil {
		s.lg.Printf("relay: 设备 %s 票据无效（来源 %s）: %v", device, r.RemoteAddr, err)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "票据无效或已过期"})
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 客户端是设备上的 Go 程序，不发 Origin，也不受浏览器 CSRF 约束；
		// 真正的门禁是上面的 HMAC 票据。
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.lg.Printf("relay: WS 升级失败（设备 %s）: %v", device, err)
		return
	}
	// ws 生命周期由 smux 会话持有：会话 Close 会关掉这条 net.Conn，进而关掉 WS。
	// watchedConn 额外负责「对端静默消失」时的快速失败（见 conn.go）。
	wc := watchConn(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	sess, err := smux.Server(wc, s.smuxConfig())
	if err != nil {
		_ = ws.CloseNow()
		s.lg.Printf("relay: smux 会话建立失败（设备 %s）: %v", device, err)
		return
	}

	ds := s.newDeviceSession(device, sess, ws, wc)
	s.attach(ds)
	s.lg.Printf("relay: 设备 %s 已连接（来源 %s，在线 %d 台）", device, r.RemoteAddr, len(s.Online()))

	defer func() {
		ds.close()
		s.detach(ds)
	}()
	<-sess.CloseChan()
	s.lg.Printf("relay: 设备 %s 已断开", device)
}

// smuxConfig 把空闲超时映射到 smux 的 keepalive：每 idle/3 发一次 NOP，
// idle 时间内没收到任何数据就判定对端死亡（拔网线、休眠的笔记本不会发 FIN）。
func (s *relayServer) smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	interval := s.idle / 3
	if interval < time.Second {
		interval = time.Second
	}
	timeout := s.idle
	if timeout < 2*interval {
		// smux 要求 timeout ≥ interval，太小会导致会话一建就被判死。
		timeout = 2 * interval
	}
	c.KeepAliveInterval = interval
	c.KeepAliveTimeout = timeout
	return c
}

func (s *relayServer) newDeviceSession(id string, sess *smux.Session, ws *websocket.Conn, wc *watchedConn) *deviceSession {
	ds := &deviceSession{id: id, sess: sess, ws: ws, conn: wc, prefix: "/t/" + id}
	ds.proxy = ds.newProxy()
	return ds
}

func (s *relayServer) attach(ds *deviceSession) {
	s.mu.Lock()
	old := s.devices[ds.id]
	s.devices[ds.id] = ds
	s.mu.Unlock()

	if old != nil {
		// 同 id 重连：顶掉旧会话，否则旧会话会一直占着流和内存。
		s.lg.Printf("relay: 设备 %s 重复注册，关闭旧会话", ds.id)
		old.close()
		s.detach(old)
	}

	// 掉线检测有两条路：
	//   1. sess.CloseChan()  —— 对端正常关闭会话；
	//   2. conn.Dead()       —— 底层 WSS 的 Read/Write 失败（拔网线、进程被杀）。
	// 只监听 (1) 会漏掉 (2)：smux 的 recvLoop 读到错误不会关闭 Session。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ds.conn.Dead():
			s.lg.Printf("relay: 设备 %s 底层连接中断", ds.id)
			ds.close()
		case <-ds.sess.CloseChan():
		case <-s.done:
			return
		}
		s.detach(ds)
	}()
}

func (s *relayServer) detach(ds *deviceSession) {
	s.mu.Lock()
	if s.devices[ds.id] == ds {
		delete(s.devices, ds.id)
	}
	s.mu.Unlock()
}

func (s *relayServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	device, _, ok := splitDevicePath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "路径应为 /t/{device}/..."})
		return
	}
	ds := s.lookup(device)
	if ds == nil {
		// 故意用 503 而不是 404：运维要能区分「设备没上线」和「路径写错」。
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "设备未连接", "device": device})
		return
	}
	// 并发流上限按「该设备在途请求数」算。CompareAndSwap 而非 Add 后比较，
	// 是为了让超限的请求不占用名额（否则突然的 200 并发会把计数推到 200）。
	for {
		cur := ds.inflight.Load()
		if cur >= s.maxStreams {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": "设备并发流超限", "limit": s.maxStreams,
			})
			return
		}
		if ds.inflight.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	defer ds.inflight.Add(-1)

	ds.proxy.ServeHTTP(w, r)
}

// splitDevicePath 从 /t/{device}/<rest> 拆出设备 id。
func splitDevicePath(p string) (device, rest string, ok bool) {
	p = strings.TrimPrefix(p, "/t/")
	if p == "" {
		return "", "", false
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		device, rest = p[:i], p[i:]
	} else {
		device, rest = p, "/"
	}
	if !deviceIDPattern.MatchString(device) {
		return "", "", false
	}
	return device, rest, true
}

func (ds *deviceSession) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			rest := strings.TrimPrefix(r.URL.Path, ds.prefix)
			if rest == "" {
				rest = "/"
			}
			if !strings.HasPrefix(rest, "/") {
				rest = "/" + rest
			}
			r.URL.Path = rest
			r.URL.RawPath = ""
			r.URL.Scheme = upstreamScheme
			r.URL.Host = upstreamHost
			r.Host = upstreamHost

			// 下游若生成绝对链接（重定向、WS 地址），需要知道自己在 /t/{device} 下。
			r.Header.Set("X-Forwarded-Prefix", ds.prefix)
			// 客户端自带的 X-Forwarded-For 一律丢弃：这是伪造入口。
			// 之后 ReverseProxy 会用 r.RemoteAddr 追加真实来源 IP。
			r.Header.Del("X-Forwarded-For")
		},
		Transport: &http.Transport{
			// 每条流都是一条新流：不复用，行为最可预测，也便于 MaxStreams 计数
			// 与「请求结束 → 流关闭」严格对应。
			DisableKeepAlives: true,
			// 上游是明文 HTTP-over-smux，不走 h2。
			ForceAttemptHTTP2: false,
			DialContext:       ds.dialStream,
			MaxIdleConns:      0,
		},
		// 控制面有帧推送（远程桌面的 JPEG/WS），必须即时 flush，
		// 否则 ReverseProxy 的缓冲会把实时性吃掉。
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "隧道不可用", "detail": err.Error(),
			})
		},
	}
}

// dialStream 让 http.Transport「连到 origin」，实际是向 smux 打开一条流。
// 剩下的 HTTP 编解码全部由 ReverseProxy/Transport 完成——包括 WS 升级。
func (ds *deviceSession) dialStream(ctx context.Context, network, addr string) (net.Conn, error) {
	if ds.sess.IsClosed() {
		return nil, ErrUnknownDevice
	}
	stream, err := ds.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("relay: 打开流失败: %w", err)
	}
	return stream, nil
}

func (ds *deviceSession) close() {
	ds.closeOnce.Do(func() {
		// 先掐 WS：可能还有 goroutine 阻塞在写一个已死的 socket 上，
		// 直接关底层连接能立刻把它们放出来，避免 sess.Close() 卡住。
		if ds.ws != nil {
			_ = ds.ws.CloseNow()
		}
		// smux 的 Close 会关掉底层 net.Conn，设备端因此立刻知道断线。
		if ds.sess != nil {
			_ = ds.sess.Close()
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ Server = (*relayServer)(nil)
