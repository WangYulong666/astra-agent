package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ── 测试脚手架 ────────────────────────────────────────────────

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("relay-log: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func testLogger(t *testing.T) *log.Logger { return log.New(testWriter{t}, "", 0) }

// origin 是「用户家里的控制面」：一个普通 HTTP 服务 + 一个 WS 端点。
type origin struct {
	addr    string
	srv     *http.Server
	hold    chan struct{}
	entered chan struct{}
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"prefix": r.Header.Get("X-Forwarded-Prefix"),
			"xff":    r.Header.Get("X-Forwarded-For"),
			"host":   r.Host,
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, append([]byte("echo:"), data...)); err != nil {
				return
			}
		}
	})
	o := &origin{hold: make(chan struct{}), entered: make(chan struct{}, 32)}
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		o.entered <- struct{}{}
		<-o.hold
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("origin 监听失败: %v", err)
	}
	o.addr = ln.Addr().String()
	o.srv = &http.Server{Handler: mux}
	go func() { _ = o.srv.Serve(ln) }()
	t.Cleanup(o.shutdown)
	return o
}

// shutdown 可重复调用（Cleanup 与用例内显式关闭都会走这里）。
func (o *origin) shutdown() {
	select {
	case <-o.hold:
	default:
		close(o.hold)
	}
	_ = o.srv.Close()
}

func newTestServer(t *testing.T, cfg ServerConfig) *relayServer {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.Logger == nil {
		cfg.Logger = testLogger(t)
	}
	s, err := OpenServer(cfg)
	if err != nil {
		t.Fatalf("OpenServer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.(*relayServer)
}

func newTestTunnel(t *testing.T, addr, token, device, local string, insecure bool) *tunnel {
	t.Helper()
	tun, err := Dial(TunnelConfig{
		URL:       "ws://" + addr,
		Token:     token,
		DeviceID:  device,
		LocalAddr: local,
		Logger:    testLogger(t),
		Insecure:  insecure,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = tun.Close() })
	return tun.(*tunnel)
}

func getJSON(t *testing.T, url string) (int, map[string]any, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // 测试
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return resp.StatusCode, m, string(body)
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", d, what)
}

// ── e2e：HTTP + WS 升级 + 未知设备 + 掉线摘除 ──────────────────

func TestE2EHTTPAndWebSocket(t *testing.T) {
	const token = "s3cret-token"
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Addr: "127.0.0.1:0", Token: token})
	tun := newTestTunnel(t, srv.Addr(), token, "dev-1", o.addr, false)

	if got := srv.Online(); len(got) != 1 || got[0] != "dev-1" {
		t.Fatalf("Online() = %v, 期望 [dev-1]", got)
	}
	if !tun.Stats().Connected {
		t.Fatal("隧道状态应为已连接")
	}

	// 1) 普通 HTTP
	code, m, body := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hello?x=1")
	if code != http.StatusOK {
		t.Fatalf("GET /t/dev-1/hello = %d, body=%s", code, body)
	}
	if m["ok"] != true {
		t.Fatalf("origin 响应应含 ok:true，实际 %v", m)
	}
	if m["path"] != "/hello" {
		t.Fatalf("origin 收到的路径应为 /hello（前缀已剥离），实际 %v", m["path"])
	}
	if m["query"] != "x=1" {
		t.Fatalf("查询串应透传，实际 %v", m["query"])
	}
	if m["prefix"] != "/t/dev-1" {
		t.Fatalf("X-Forwarded-Prefix = %v, 期望 /t/dev-1", m["prefix"])
	}
	t.Logf("① HTTP 经 relay → origin：%d %s", code, body)

	// 2) WebSocket 升级：ReverseProxy 原生处理 101 后的双向 copy
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws://" + srv.Addr() + "/t/dev-1/ws"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("经 relay 建立 WS 失败: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("WS 写失败: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("WS 读失败: %v", err)
	}
	if string(data) != "echo:ping" || typ != websocket.MessageText {
		t.Fatalf("回显不符：typ=%v data=%q", typ, data)
	}
	t.Logf("② WS 经 relay 回显：%q", data)
	_ = c.Close(websocket.StatusNormalClosure, "")

	// 3) 未知设备 → 503（不是 404）
	code, _, body = getJSON(t, "http://"+srv.Addr()+"/t/unknown/hello")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("未知设备应 503，实际 %d body=%s", code, body)
	}
	if !strings.Contains(body, "未连接") {
		t.Fatalf("503 body 应含「未连接」，实际 %s", body)
	}
	t.Logf("③ 未知设备：%d %s", code, body)

	// 4) 设备断开 → 注册表摘除 → 503
	if err := tun.Close(); err != nil {
		t.Fatalf("隧道 Close: %v", err)
	}
	waitFor(t, "设备从注册表摘除", 3*time.Second, func() bool { return len(srv.Online()) == 0 })
	code, _, body = getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hello")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "未连接") {
		t.Fatalf("设备下线后应 503，实际 %d body=%s", code, body)
	}
	t.Logf("④ 设备下线后：%d %s", code, body)
}

func TestHealthz(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk"})

	code, m, body := getJSON(t, "http://"+srv.Addr()+"/_relay/healthz")
	if code != http.StatusOK || m["ok"] != true {
		t.Fatalf("healthz = %d %s", code, body)
	}
	if m["devices"] != float64(0) {
		t.Fatalf("devices 应为 0，实际 %v", m["devices"])
	}

	newTestTunnel(t, srv.Addr(), "tk", "dev-9", o.addr, false)
	_, m, _ = getJSON(t, "http://"+srv.Addr()+"/_relay/healthz")
	if m["devices"] != float64(1) {
		t.Fatalf("devices 应为 1，实际 %v", m["devices"])
	}
}

// ── 并发 ────────────────────────────────────────────────────

func TestConcurrentRequests(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk"})
	newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)

	var wg sync.WaitGroup
	errs := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, m, body := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hello")
			if code != http.StatusOK || m["ok"] != true {
				errs <- fmt.Sprintf("第 %d 个请求失败: %d %s", i, code, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestMaxStreams(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk", MaxStreams: 2})
	newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hold")
			codes <- code
		}()
	}
	// 等到 origin 真的进了两个请求（即两条流在用）
	for i := 0; i < 2; i++ {
		select {
		case <-o.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("两个占位请求未到达 origin")
		}
	}
	code, _, body := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hold")
	if code != http.StatusTooManyRequests {
		t.Fatalf("第 3 个并发请求应 429，实际 %d body=%s", code, body)
	}
	t.Logf("第 3 个并发流：%d %s", code, body)

	close(o.hold)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("占位请求应 200，实际 %d", c)
		}
	}
}

// ── 重连 ────────────────────────────────────────────────────

func TestReconnect(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk"})
	tun := newTestTunnel(t, srv.Addr(), "tk", "dev-1", o.addr, false)

	// relay 侧主动踢掉会话，模拟 relay 重启 / NAT 超时后的断链。
	srv.mu.RLock()
	ds := srv.devices["dev-1"]
	srv.mu.RUnlock()
	if ds == nil {
		t.Fatal("注册表里没有 dev-1")
	}
	ds.close()

	waitFor(t, "设备 2 秒内重连", 2*time.Second, func() bool {
		return tun.Stats().Reconnects >= 1 && len(srv.Online()) == 1
	})
	waitFor(t, "重连后隧道可用", 3*time.Second, func() bool {
		code, _, _ := getJSON(t, "http://"+srv.Addr()+"/t/dev-1/hello")
		return code == http.StatusOK
	})
	t.Logf("重连统计：%+v", tun.Stats())
}

// relay 先关：设备端必须感知断线（而不是挂着等 keepalive），
// relay 的 Close 也不能被阻塞在隧道处理协程上。
func TestRelayCloseDisconnectsTunnel(t *testing.T) {
	o := newOrigin(t)
	srv, err := OpenServer(ServerConfig{Addr: "127.0.0.1:0", Token: "tk", Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("OpenServer: %v", err)
	}
	tun, err := Dial(TunnelConfig{
		URL: "ws://" + srv.Addr(), Token: "tk", DeviceID: "dev-1",
		LocalAddr: o.addr, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer tun.Close()

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("relay Close: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("relay Close 耗时 %v，疑似被隧道协程阻塞", d)
	}
	waitFor(t, "设备端感知断线", 2*time.Second, func() bool { return !tun.Stats().Connected })
}

// ── TLS（wss + 自签证书 + Insecure）──────────────────────────

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("自签证书: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
}

func TestTLSTunnel(t *testing.T) {
	o := newOrigin(t)
	srv := newTestServer(t, ServerConfig{Token: "tk", TLS: selfSignedTLS(t)})

	tun, err := Dial(TunnelConfig{
		URL: "wss://" + srv.Addr(), Token: "tk", DeviceID: "dev-tls",
		LocalAddr: o.addr, Logger: testLogger(t), Insecure: true,
	})
	if err != nil {
		t.Fatalf("wss 建隧道失败: %v", err)
	}
	defer tun.Close()

	// relay 自己终结 TLS 时，手机侧也应走 https
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := client.Get("https://" + srv.Addr() + "/t/dev-tls/hello")
	if err != nil {
		t.Fatalf("https 请求失败: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := decodeJSON(resp.Body, &m); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if resp.StatusCode != http.StatusOK || m["ok"] != true {
		t.Fatalf("TLS 链路响应异常: %d %v", resp.StatusCode, m)
	}
	t.Logf("wss/https 经 relay：%d %v", resp.StatusCode, m["ok"])
}

// ── Close 幂等 + 无 goroutine 泄漏 ───────────────────────────
func TestCloseIdempotentNoLeak(t *testing.T) {
	// 先跑一轮「预热」，让 http 客户端连接池等一次性 goroutine 先建起来。
	round := func() {
		o := newOrigin(t)
		srv, err := OpenServer(ServerConfig{Addr: "127.0.0.1:0", Token: "tk", Logger: testLogger(t)})
		if err != nil {
			t.Fatalf("OpenServer: %v", err)
		}
		tun, err := Dial(TunnelConfig{
			URL: "ws://" + srv.Addr(), Token: "tk", DeviceID: "dev-x",
			LocalAddr: o.addr, Logger: testLogger(t),
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if code, _, _ := getJSON(t, "http://"+srv.Addr()+"/t/dev-x/hello"); code != http.StatusOK {
			t.Fatalf("预热请求失败: %d", code)
		}
		if err := tun.Close(); err != nil {
			t.Fatalf("Close 隧道: %v", err)
		}
		if err := tun.Close(); err != nil {
			t.Fatalf("Close 应可重复调用: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("Close relay: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Fatalf("Close relay 应可重复调用: %v", err)
		}
		o.shutdown()
	}
	round()
	time.Sleep(200 * time.Millisecond)

	before := runtime.NumGoroutine()
	// 多跑几轮：单轮漏 1-2 个 goroutine 会被余量吃掉，跑 5 轮才看得出来。
	const rounds = 5
	for i := 0; i < rounds; i++ {
		round()
	}
	// 给连接关闭后的清理留点时间。
	deadline := time.Now().Add(3 * time.Second)
	after := runtime.NumGoroutine()
	for time.Now().Before(deadline) && after > before+10 {
		time.Sleep(50 * time.Millisecond)
		after = runtime.NumGoroutine()
	}
	if after > before+10 {
		t.Fatalf("疑似 goroutine 泄漏：%d 轮后 before=%d after=%d", rounds, before, after)
	}
	t.Logf("goroutine：%d 轮前=%d 后=%d", rounds, before, after)
}
