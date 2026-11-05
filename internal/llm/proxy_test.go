package llm

// 网关单测。上游一律用 httptest 假服务，不碰真实网络、不需要 key。
//
// 这四个用例对应四件「一旦错了就会静默出错」的事：
//   - usage 解析/成本算错  → 账单错，而且没人会发现
//   - 流式不逐块 Flush    → 前端看不到逐字输出（功能坏了但请求是 200）
//   - 上游状态码被改写    → 排查方向被带偏
//   - 客户端断开就丢账    → 钱花了但没记账

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestProxy 起一个假上游 + 一个指向它的 Proxy。
func newTestProxy(t *testing.T, upstream http.HandlerFunc) (*Proxy, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	p := &Proxy{
		Registry: NewRegistry(Config{Providers: map[string]Provider{
			ProviderDeepSeek: {Name: ProviderDeepSeek, BaseURL: srv.URL, APIKey: "sk-real"},
		}}),
		Pricing: NewPricing(nil),
	}
	return p, srv
}

func TestServeNonStreamUsageAndCost(t *testing.T) {
	var gotAuth, gotPath string
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-1",
			"model": "deepseek-chat",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}},
			},
			"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 340},
		})
	})

	var (
		mu       sync.Mutex
		cbModel  string
		cbIn     int64
		cbOut    int64
		cbCalled int
	)
	p.OnUsage = func(model string, in, out int64) {
		mu.Lock()
		defer mu.Unlock()
		cbModel, cbIn, cbOut = model, in, out
		cbCalled++
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/llm/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec, req, Meta{TenantID: "t1", SessionID: "s1"}); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	// 真 key 必须由网关塞进去，且虚拟 key 不能漏给上游。
	if gotAuth != "Bearer sk-real" {
		t.Fatalf("上游应收到真 key，实际 %q", gotAuth)
	}
	// 前缀必须剥掉：/v1/llm/v1/chat/completions → /v1/chat/completions
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径应为 /v1/chat/completions，实际 %q", gotPath)
	}

	mu.Lock()
	defer mu.Unlock()
	if cbCalled != 1 {
		t.Fatalf("usage 回调应恰好一次，实际 %d 次", cbCalled)
	}
	if cbIn != 1200 || cbOut != 340 {
		t.Fatalf("token 解析错误: in=%d out=%d", cbIn, cbOut)
	}
	if cbModel != "deepseek-chat" {
		t.Fatalf("model 应为 deepseek-chat，实际 %q", cbModel)
	}

	price, _ := p.Pricing.Price("deepseek-chat")
	want := Cost(price, 1200, 340)
	if got := Cost(price, cbIn, cbOut); got != want {
		t.Fatalf("成本应为 %d 微美元，实际 %d", want, got)
	}
	// 手算一遍，确认 Cost 不是自证自明：
	// 1200 输入 + 340 输出，按占位价 300000/1200000 微美元每百万：
	// (1200*300000 + 340*1200000) / 1e6 = (360000000 + 408000000)/1e6 = 768
	if want != 768 {
		t.Fatalf("按占位价手算应为 768 微美元，实际 %d", want)
	}
}

// TestServeStreamInjectsUsageAndFlushes 覆盖流式三件事：
// 注入 include_usage、逐帧透传、末帧 usage 记账。
func TestServeStreamInjectsUsageAndFlushes(t *testing.T) {
	const (
		chunkGap = 60 * time.Millisecond
	)
	var (
		upstreamBody string
		mu           sync.Mutex
		finishedAt   time.Time
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = string(b)
		mu.Unlock()

		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("假上游需要 Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		for i := 1; i <= 2; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}],\"usage\":null}\n\n", i)
			fl.Flush()
			time.Sleep(chunkGap)
		}
		// 末帧：DeepSeek 在 include_usage=true 时把 usage 放在这一帧。
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"deepseek-chat\",\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		mu.Lock()
		finishedAt = time.Now()
		mu.Unlock()
	}))
	defer upstream.Close()

	p := &Proxy{
		Registry: NewRegistry(Config{Providers: map[string]Provider{
			ProviderDeepSeek: {BaseURL: upstream.URL, APIKey: "sk-real"},
		}}),
		Pricing: NewPricing(nil),
	}
	var (
		cbMu    sync.Mutex
		cbIn    int64
		cbOut   int64
		cbCalls int
	)
	p.OnUsage = func(_ string, in, out int64) {
		cbMu.Lock()
		defer cbMu.Unlock()
		cbIn, cbOut = in, out
		cbCalls++
	}

	// 经过真实 HTTP 往返，才能观察到「客户端在第几毫秒收到字节」。
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.Serve(r.Context(), w, r, Meta{TenantID: "t1"}); err != nil {
			t.Errorf("Serve 失败: %v", err)
		}
	}))
	defer proxySrv.Close()

	body := `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxySrv.URL+"/v1/llm/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}

	// 逐行读，记录第一块到达的时刻。
	br := bufio.NewReader(resp.Body)
	var firstAt time.Time
	var seen []string
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			if firstAt.IsZero() && strings.Contains(line, "tok1") {
				firstAt = time.Now()
			}
			if strings.HasPrefix(line, "data: {") {
				seen = append(seen, line)
			}
		}
		if rerr != nil {
			break
		}
	}

	// 1) 注入生效：上游确实收到了 include_usage。
	mu.Lock()
	up := upstreamBody
	fin := finishedAt
	mu.Unlock()
	var sent struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(up), &sent); err != nil {
		t.Fatalf("上游收到的请求体不是 JSON: %v (%s)", err, up)
	}
	if !sent.Stream || !sent.StreamOptions.IncludeUsage {
		t.Fatalf("必须注入 stream_options.include_usage=true，上游实际收到: %s", up)
	}

	// 2) 逐帧透传：客户端拿到第一块时上游还没结束。
	if firstAt.IsZero() {
		t.Fatal("客户端没收到第一块内容")
	}
	if !firstAt.Before(fin) {
		t.Fatalf("首块到达(%v)不早于上游结束(%v)，说明被攒起来了", firstAt, fin)
	}
	if gap := fin.Sub(firstAt); gap < chunkGap/2 {
		t.Fatalf("首块与上游结束只差 %v，太接近，疑似缓冲", gap)
	}
	if len(seen) != 3 {
		t.Fatalf("应透传 3 个 data 帧，实际 %d: %v", len(seen), seen)
	}

	// 3) 末帧 usage 被记账。
	cbMu.Lock()
	defer cbMu.Unlock()
	if cbCalls != 1 {
		t.Fatalf("usage 回调应恰好一次，实际 %d 次", cbCalls)
	}
	if cbIn != 42 || cbOut != 7 {
		t.Fatalf("token 解析错误: in=%d out=%d", cbIn, cbOut)
	}
}

// TestServePassesUpstreamErrorVerbatim 401 必须原样出去：把 401 变成 500
// 会让排查方向从「key 错了」变成「网关挂了」。
func TestServePassesUpstreamErrorVerbatim(t *testing.T) {
	const errBody = `{"error":{"message":"Authentication Fails","type":"authentication_error"}}`
	p, srv := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errBody)
	})
	_ = srv

	var called int
	p.OnUsage = func(string, int64, int64) { called++ }

	req := httptest.NewRequest(http.MethodPost, "/v1/llm/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat"}`))
	rec := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec, req, Meta{}); err != nil {
		t.Fatalf("上游已响应时不应返回 error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码应原样透传 401，实际 %d", rec.Code)
	}
	if got := rec.Body.String(); got != errBody {
		t.Fatalf("响应体应原样透传，实际 %q", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("上游响应头应保留，X-Request-Id=%q", got)
	}
	if called != 0 {
		t.Fatalf("失败请求不该记账，实际回调 %d 次", called)
	}
}

// dropWriter 模拟「客户端读完第一块就走了」：后续写入一律失败。
type dropWriter struct {
	cancel   context.CancelFunc
	header   http.Header
	attempts int // 写入尝试次数（含失败）
	success  int // 真正写出去的次数
	code     int
}

func (d *dropWriter) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}
func (d *dropWriter) WriteHeader(code int) { d.code = code }
func (d *dropWriter) Write(p []byte) (int, error) {
	d.attempts++
	if d.attempts > 1 {
		return 0, errors.New("客户端已断开")
	}
	d.success++
	d.cancel() // 第一帧写出去之后断开
	return len(p), nil
}
func (d *dropWriter) Flush() {}

// TestServeRecordsUsageWhenClientDisconnects 客户端断开后仍要产生一次 usage 回调：
// 断开的是连接，不是已经生成的 token。
func TestServeRecordsUsageWhenClientDisconnects(t *testing.T) {
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}],\"usage\":null}\n\n", i)
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		// 客户端走掉之后上游仍然把这一轮跑完了。
		fmt.Fprint(w, "data: {\"model\":\"deepseek-chat\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	cb := make(chan [3]int64, 4)
	p.OnUsage = func(model string, in, out int64) {
		cb <- [3]int64{in, out, int64(len(model))}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dw := &dropWriter{cancel: cancel}

	req := httptest.NewRequest(http.MethodPost, "/v1/llm/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-chat","stream":true}`))
	if err := p.Serve(ctx, dw, req, Meta{}); err != nil {
		t.Fatalf("Serve 不该因客户端断开而报错: %v", err)
	}

	select {
	case got := <-cb:
		if got[0] != 100 || got[1] != 7 {
			t.Fatalf("token 应为 100/7，实际 %d/%d", got[0], got[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("客户端断开后没有产生 usage 回调——账丢了")
	}

	// 断开后不应再往客户端写（写也写不进去）：只允许第一帧成功。
	if dw.success != 1 {
		t.Fatalf("断开后仍在写客户端，成功写入 %d 次", dw.success)
	}
	// 也不能重复记账。
	select {
	case extra := <-cb:
		t.Fatalf("产生了重复记账: %v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}
