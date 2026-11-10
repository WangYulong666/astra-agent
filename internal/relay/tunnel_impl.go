package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/xtaci/smux"
)

const (
	// 单隧道允许同时在途的本地连接数（与 relay 的 MaxStreams 对齐）。
	defaultClientStreams = 128
	// 单次建连的超时（含 WS 握手 + smux 初始化）。
	connectTimeout = 15 * time.Second
	reconnectMin   = time.Second
	reconnectMax   = 30 * time.Second
	localDialTO    = 10 * time.Second
)

// dialImpl 建立隧道。首次建连是**同步**的：上层（app.startTunnel）要在启动日志里
// 说清楚「连上了没有」，异步返回会让它永远打印成功。
// 连上之后的断线由后台循环负责，指数退避重连，直到 Close。
func dialImpl(cfg TunnelConfig) (Tunnel, error) {
	wsURL, err := normalizeURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.LocalAddr) == "" {
		return nil, errors.New("relay: LocalAddr 不能为空")
	}
	if !deviceIDPattern.MatchString(cfg.DeviceID) {
		return nil, fmt.Errorf("relay: DeviceID 非法（只允许字母数字和 ._-，最长 64）: %q", cfg.DeviceID)
	}

	lg := cfg.Logger
	if lg == nil {
		lg = log.New(os.Stderr, "relay-tunnel ", log.LstdFlags)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t := &tunnel{
		cfg:     cfg,
		lg:      lg,
		wsURL:   wsURL,
		ctx:     ctx,
		cancel:  cancel,
		streams: make(chan struct{}, defaultClientStreams),
	}

	if err := t.connect(); err != nil {
		cancel()
		return nil, err
	}

	t.wg.Add(1)
	go t.run()
	lg.Printf("隧道已建立：%s（设备 %s → %s）", wsURL, cfg.DeviceID, cfg.LocalAddr)
	return t, nil
}

type tunnel struct {
	cfg   TunnelConfig
	lg    *log.Logger
	wsURL string

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	sess       *smux.Session
	ws         *websocket.Conn
	connected  bool
	since      time.Time
	reconnects int
	lastErr    string
	closing    bool

	// streams 是并发连接配额（信号量），wg 用来等所有流退出。
	streams chan struct{}
	wg      sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// normalizeURL 接受 ws/wss（顺带容忍 http/https 写法），去掉末尾的 "/"。
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("relay: URL 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("relay: URL 非法: %w", err)
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "ws"
	case "wss", "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("relay: URL 只支持 ws:// 或 wss://，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("relay: URL 缺少主机名")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// connect 做一次建连尝试。失败不改 Stats 的 Connected，只记 LastError。
func (t *tunnel) connect() error {
	ctx, cancel := context.WithTimeout(t.ctx, connectTimeout)
	defer cancel()

	dialURL := t.wsURL + ticketPath(t.cfg.DeviceID, t.cfg.Token, time.Now())

	opts := &websocket.DialOptions{}
	if strings.HasPrefix(dialURL, "wss://") {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					// 仅本机验证用；正式环境必须让证书校验生效。
					InsecureSkipVerify: t.cfg.Insecure,
					MinVersion:         tls.VersionTLS12,
					ServerName:         serverName(t.wsURL),
				},
				DisableKeepAlives: true,
				ForceAttemptHTTP2: false,
			},
		}
	}

	ws, resp, err := websocket.Dial(ctx, dialURL, opts)
	if err != nil {
		msg := err.Error()
		if resp != nil {
			msg = fmt.Sprintf("%s（HTTP %d）", msg, resp.StatusCode)
		}
		t.setLastErr(msg)
		return fmt.Errorf("relay: 连接 %s 失败: %s", t.wsURL, msg)
	}
	// 握手完成后的读写不该被 connectTimeout 掐断：换到长生命周期 ctx。
	nc := websocket.NetConn(t.ctx, ws, websocket.MessageBinary)
	sess, err := smux.Client(nc, t.smuxConfig())
	if err != nil {
		_ = ws.CloseNow()
		t.setLastErr(err.Error())
		return fmt.Errorf("relay: 初始化 smux 失败: %w", err)
	}

	t.mu.Lock()
	t.sess = sess
	t.ws = ws
	t.connected = true
	t.since = time.Now()
	t.lastErr = ""
	t.mu.Unlock()
	return nil
}

func (t *tunnel) smuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	// 客户端比服务端勤快一点：服务端 90s 空闲超时，这里 20s 一个 NOP，
	// 既能让 NAT/反代不掐连接，也能在 60s 内发现对端已死。
	c.KeepAliveInterval = 20 * time.Second
	c.KeepAliveTimeout = 60 * time.Second
	return c
}

// run 是隧道的主循环：等待当前会话结束 → 退避重连，直到 Close。
func (t *tunnel) run() {
	defer t.wg.Done()
	for {
		sess := t.currentSession()
		if sess != nil {
			t.acceptLoop(sess)
			// 会话已不可用：显式 Close 一把，否则 smux 内部的 keepalive/sendLoop
			// 协程会一直挂着（recvLoop 出错不会自动关 Session）。
			_ = sess.Close()
		}
		if t.ctx.Err() != nil {
			return
		}
		t.markDown()
		if !t.reconnect() {
			return
		}
	}
}

func (t *tunnel) currentSession() *smux.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sess
}

func (t *tunnel) markDown() {
	t.mu.Lock()
	t.connected = false
	t.sess = nil
	t.mu.Unlock()
}

// acceptLoop 接收 relay 打开的逻辑流，每条流接到本地一条 TCP。
// 会话死亡时 AcceptStream 返回错误，这里就退出，交给 run 重连。
func (t *tunnel) acceptLoop(sess *smux.Session) {
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			t.setLastErr(err.Error())
			return
		}
		select {
		case t.streams <- struct{}{}:
		default:
			// 本地配额打满：直接关流，relay 侧看到的是 502，
			// 比让请求排无限长的队更诚实。
			t.lg.Printf("隧道: 并发连接已满（%d），拒绝新流", defaultClientStreams)
			_ = st.Close()
			continue
		}

		t.mu.Lock()
		if t.closing {
			t.mu.Unlock()
			<-t.streams
			_ = st.Close()
			continue
		}
		t.wg.Add(1)
		t.mu.Unlock()

		go func(st *smux.Stream) {
			defer t.wg.Done()
			defer func() { <-t.streams }()
			t.serveStream(st)
		}(st)
	}
}

// serveStream 把一条 smux 流接到一条本地 TCP 连接上。
// 两个方向各一个 io.Copy；任一方向结束就用 CloseWrite 通知对端 EOF
// （HTTP 的 Content-Length / WS 的关闭帧都依赖半关闭），然后再整体关闭。
func (t *tunnel) serveStream(st *smux.Stream) {
	defer st.Close()

	conn, err := net.DialTimeout("tcp", t.cfg.LocalAddr, localDialTO)
	if err != nil {
		t.setLastErr(fmt.Sprintf("本地 %s 连接失败: %v", t.cfg.LocalAddr, err))
		return
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, st)
		closeWrite(conn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(st, conn)
		closeWrite(st)
	}()
	wg.Wait()
}

// closeWrite 尽力做半关闭；不支持的类型就跳过，最终由 Close 兜底。
func closeWrite(c io.ReadWriteCloser) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// reconnect 指数退避重连，成功返回 true；Close 后返回 false。
// 1s → 2s → 4s → … → 30s，每次叠加 ±20% 抖动，避免大量设备同时回连打爆 relay。
func (t *tunnel) reconnect() bool {
	backoff := reconnectMin
	for {
		delay := jitter(backoff)
		timer := time.NewTimer(delay)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}

		err := t.connect()
		if err == nil {
			t.mu.Lock()
			t.reconnects++
			t.mu.Unlock()
			t.lg.Printf("隧道已重连：%s（第 %d 次）", t.wsURL, t.Stats().Reconnects)
			return true
		}
		t.lg.Printf("隧道重连失败，%.1fs 后重试: %v", backoff.Seconds(), err)

		backoff *= 2
		if backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

func jitter(d time.Duration) time.Duration {
	// 用 time.Now().UnixNano() 的低位当随机源，避免引入 math/rand 全局锁。
	n := time.Now().UnixNano()
	delta := float64(d) * 0.2
	// [0,1) → [-0.2, +0.2)
	f := float64(n%1000)/1000*2 - 1
	return d + time.Duration(f*delta)
}

func (t *tunnel) setLastErr(msg string) {
	t.mu.Lock()
	t.lastErr = msg
	t.mu.Unlock()
}

func (t *tunnel) Stats() TunnelStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := TunnelStats{
		Connected:  t.connected,
		URL:        t.wsURL,
		DeviceID:   t.cfg.DeviceID,
		Reconnects: t.reconnects,
		Streams:    len(t.streams),
		LastError:  t.lastErr,
	}
	if !t.since.IsZero() {
		st.Since = t.since
	}
	return st
}

func (t *tunnel) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		sess := t.sess
		ws := t.ws
		t.connected = false
		t.sess = nil
		t.mu.Unlock()

		t.cancel()
		if sess != nil {
			_ = sess.Close()
		}
		if ws != nil {
			_ = ws.CloseNow()
		}
		// 等 accept 循环、在途流、重连协程全部退出，不留孤儿 goroutine。
		t.wg.Wait()
		t.lg.Printf("隧道已关闭：%s", t.wsURL)
	})
	return t.closeErr
}

func serverName(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	h := u.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.Trim(h, "[]")
}

var _ Tunnel = (*tunnel)(nil)
