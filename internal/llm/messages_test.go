package llm

// Messages 协议（dsh 实际走的那条）的单测。上游仍是 httptest 假服务。
//
// 这里守的是四件「错了不会报错、只会悄悄算错账」的事：
//   - Messages 的 usage 分散在首尾两帧，合并规则错 → 每条账都记错
//   - message_delta 的 output_tokens 是累计值，当增量累加 → 输出被算两次
//   - 给 Messages 请求注入 stream_options → 上游可能直接 400，链路整条不通
//   - 子路径（count_tokens）被判成透明转发 → 那类请求一条账都不记

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDetectProtocol(t *testing.T) {
	cases := []struct {
		path string
		want Protocol
	}{
		// Messages：带前缀 / 不带前缀 / 带尾斜杠 / 带 query 都应归到同一族。
		{"/v1/llm/anthropic/v1/messages", ProtocolMessages},
		{"/anthropic/v1/messages", ProtocolMessages},
		{"/v1/messages", ProtocolMessages},
		{"/messages", ProtocolMessages},
		{"/v1/messages/", ProtocolMessages},
		{"/v1/messages?beta=true", ProtocolMessages},
		// 子路径同族：同一套事件、同一套 usage 语义。
		{"/v1/messages/count_tokens", ProtocolMessages},
		{"/anthropic/v1/messages/count_tokens/", ProtocolMessages},

		// OpenAI：行为必须与改造前完全一致。
		{"/v1/llm/v1/chat/completions", ProtocolOpenAI},
		{"/v1/chat/completions", ProtocolOpenAI},
		{"/chat/completions", ProtocolOpenAI},
		{"/v1/chat/completions/", ProtocolOpenAI},

		// 其余一律透明转发：看不懂的东西不要假装能记账。
		{"/v1/models", ProtocolTransparent},
		{"/v1/files", ProtocolTransparent},
		{"/v1/embeddings", ProtocolTransparent},
		{"/", ProtocolTransparent},
		{"", ProtocolTransparent},
		// 前缀相近但不是同一个词，不能被前缀匹配带偏。
		{"/v1/modelsx", ProtocolTransparent},
		{"/v1/messagesx", ProtocolTransparent},
	}
	for _, c := range cases {
		if got := detectProtocol(c.path); got != c.want {
			t.Errorf("detectProtocol(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}

// TestTargetPathStripsPrefix 确认 dsh 的 baseURL 拼出来的路径能落到上游：
// /v1/llm/anthropic/v1/messages - /v1/llm = /anthropic/v1/messages，
// 与 https://api.deepseek.com/anthropic/v1/messages 正好接上。
func TestTargetPathStripsPrefix(t *testing.T) {
	cases := []struct {
		prefix string
		url    string
		want   string
	}{
		{"", "/v1/llm/anthropic/v1/messages", "/anthropic/v1/messages"},
		{"", "/v1/llm/v1/chat/completions", "/v1/chat/completions"},
		{"", "/v1/llm/v1/messages?beta=1", "/v1/messages?beta=1"},
		{"", "/v1/llm/models", "/models"},
		{"/gw", "/gw/v1/messages", "/v1/messages"},
		{"/gw/", "/gw/v1/messages", "/v1/messages"},
	}
	for _, c := range cases {
		p := &Proxy{PathPrefix: c.prefix}
		r := httptest.NewRequest(http.MethodPost, c.url, nil)
		if got := p.targetPath(r); got != c.want {
			t.Errorf("targetPath(prefix=%q, %q) = %q，期望 %q", c.prefix, c.url, got, c.want)
		}
	}
}

// usageRecorder 收集回调，避免测试里到处写锁。
type usageRecorder struct {
	mu     sync.Mutex
	calls  int
	in     int64
	out    int64
	models []string
}

func (u *usageRecorder) fn(model string, in, out int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.in, u.out = in, out
	u.models = append(u.models, model)
}

func (u *usageRecorder) snapshot() (calls int, in, out int64, models []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.in, u.out, append([]string(nil), u.models...)
}

// TestMessagesNonStreamUsage 非流式：usage 在响应体顶层，input/output 都是完整值。
func TestMessagesNonStreamUsage(t *testing.T) {
	const upstreamResp = `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-chat","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":42}}`

	var (
		mu        sync.Mutex
		gotPath   string
		gotAuth   string
		gotXAPI   string
		gotApiKey string
		gotBody   string
	)
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotXAPI = r.Header.Get("X-Api-Key")
		gotApiKey = r.Header.Get("Api-Key")
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamResp)
	})

	var rec usageRecorder
	p.OnUsage = rec.fn

	// 客户端按 Anthropic SDK 的惯例用 x-api-key 带虚拟 key。
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/anthropic/v1/messages",
		strings.NewReader(`{"model":"deepseek-chat","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", "vk-virtual")
	req.Header.Set("Authorization", "Bearer vk-virtual")
	rec2 := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec2, req, Meta{TenantID: "t1"}); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}

	if rec2.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d，body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != upstreamResp {
		t.Fatalf("响应体应原样透传，实际 %s", rec2.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	// 路径：/v1/llm 前缀剥掉，/anthropic 保留给上游当路径的一部分。
	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("上游路径应为 /anthropic/v1/messages，实际 %q", gotPath)
	}
	// 认证头双写：两种上游实现各认一个字段。
	if gotAuth != "Bearer sk-real" {
		t.Fatalf("Authorization 应为真 key，实际 %q", gotAuth)
	}
	if gotXAPI != "sk-real" {
		t.Fatalf("x-api-key 应为真 key，实际 %q", gotXAPI)
	}
	// 虚拟 key 不能出现在任何一个 key 头里。
	if strings.Contains(gotAuth+gotXAPI+gotApiKey, "vk-virtual") {
		t.Fatalf("虚拟 key 泄漏到上游: auth=%q x-api-key=%q api-key=%q", gotAuth, gotXAPI, gotApiKey)
	}
	// 非流式请求不该被整形。
	if strings.Contains(gotBody, "stream_options") {
		t.Fatalf("非流式请求不该出现 stream_options: %s", gotBody)
	}

	calls, in, out, models := rec.snapshot()
	if calls != 1 {
		t.Fatalf("usage 回调应恰好一次，实际 %d 次", calls)
	}
	if in != 9 || out != 42 {
		t.Fatalf("token 应为 9/42，实际 %d/%d", in, out)
	}
	if len(models) != 1 || models[0] != "deepseek-chat" {
		t.Fatalf("model 应为 deepseek-chat，实际 %v", models)
	}
}

// TestMessagesStreamUsageAndNoStreamOptions 流式主链路：
// 逐帧透传、首尾两帧 usage 合并成一次记账、且绝不注入 stream_options。
func TestMessagesStreamUsageAndNoStreamOptions(t *testing.T) {
	frames := []string{
		`event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","model":"deepseek-chat","usage":{"input_tokens":11,"output_tokens":1}}}` + "\n\n",
		`event: content_block_start` + "\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n",
		`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n",
		// 累计值 42，不是增量。累加的实现会记成 43（首帧占位 1 + 42）。
		`event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n",
		// message_stop 不带 usage，忽略即可，但必须原样透传。
		`event: message_stop` + "\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	}
	want := strings.Join(frames, "")

	var (
		mu       sync.Mutex
		upstream string
	)
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstream = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			w.(http.Flusher).Flush()
		}
	})

	var rec usageRecorder
	p.OnUsage = rec.fn

	req := httptest.NewRequest(http.MethodPost, "/v1/llm/anthropic/v1/messages",
		strings.NewReader(`{"model":"deepseek-chat","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec2 := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec2, req, Meta{}); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}

	// 1) 字节级透传：event: 与 data: 行都不能被改写或丢掉。
	if got := rec2.Body.String(); got != want {
		t.Fatalf("客户端收到的流与上游不一致\n实际: %q\n期望: %q", got, want)
	}

	// 2) 绝不注入 stream_options —— Anthropic 端点不认这个字段，注入了可能直接 400。
	mu.Lock()
	sent := upstream
	mu.Unlock()
	if strings.Contains(sent, "stream_options") {
		t.Fatalf("Messages 请求被注入了 stream_options: %s", sent)
	}
	// 顺带确认请求体其它字段没被改动。
	var shape struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		MaxToken int    `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(sent), &shape); err != nil {
		t.Fatalf("上游收到的请求体不是 JSON: %v (%s)", err, sent)
	}
	if shape.Model != "deepseek-chat" || !shape.Stream || shape.MaxToken != 64 {
		t.Fatalf("请求体被意外改动: %s", sent)
	}

	// 3) 首尾两帧合并成一次记账。
	calls, in, out, models := rec.snapshot()
	if calls != 1 {
		t.Fatalf("usage 回调应恰好一次（首尾帧各记一次就说明没做合并），实际 %d 次", calls)
	}
	if in != 11 {
		t.Fatalf("input_tokens 应来自 message_start = 11，实际 %d", in)
	}
	if out != 42 {
		t.Fatalf("output_tokens 应取 message_delta 的累计值 42，实际 %d", out)
	}
	if len(models) != 1 || models[0] != "deepseek-chat" {
		t.Fatalf("model 应来自 message_start.message.model，实际 %v", models)
	}
}

// TestMessagesStreamFlushesEachBlock 长文本逐块输出：
// 客户端收到的 content 顺序必须与上游一致，且第一块要在上游结束前到达
// （攒完再发的话 agent 会「卡十几秒然后整段蹦出来」）。
func TestMessagesStreamFlushesEachBlock(t *testing.T) {
	const chunkGap = 60 * time.Millisecond
	chunks := []string{"A", "B", "C", "D"}

	var (
		mu         sync.Mutex
		finishedAt time.Time
	)
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		_, _ = io.WriteString(w, `event: message_start`+"\n"+
			`data: {"type":"message_start","message":{"model":"deepseek-chat","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n")
		fl.Flush()
		for _, c := range chunks {
			_, _ = io.WriteString(w, `event: content_block_delta`+"\n"+
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+c+`"}}`+"\n\n")
			fl.Flush()
			time.Sleep(chunkGap)
		}
		_, _ = io.WriteString(w, `event: message_delta`+"\n"+
			`data: {"type":"message_delta","usage":{"output_tokens":4}}`+"\n\n")
		_, _ = io.WriteString(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
		fl.Flush()

		mu.Lock()
		finishedAt = time.Now()
		mu.Unlock()
	})

	var rec usageRecorder
	p.OnUsage = rec.fn

	// 走真实 HTTP 往返，才能观察到「客户端在第几毫秒收到字节」。
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.Serve(r.Context(), w, r, Meta{}); err != nil {
			t.Errorf("Serve 失败: %v", err)
		}
	}))
	defer proxySrv.Close()

	body := `{"model":"deepseek-chat","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxySrv.URL+"/v1/llm/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	var (
		firstAt  time.Time
		gotOrder []string
	)
	for {
		line, rerr := br.ReadString('\n')
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "content_block_delta") {
			var ev struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &ev); err != nil {
				t.Fatalf("delta 帧不是 JSON: %v (%s)", err, line)
			}
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			gotOrder = append(gotOrder, ev.Delta.Text)
		}
		if rerr != nil {
			break
		}
	}

	if strings.Join(gotOrder, "") != strings.Join(chunks, "") {
		t.Fatalf("内容顺序应与上游一致，实际 %v，期望 %v", gotOrder, chunks)
	}

	mu.Lock()
	fin := finishedAt
	mu.Unlock()
	if firstAt.IsZero() {
		t.Fatal("客户端没收到任何 content_block_delta")
	}
	if !firstAt.Before(fin) {
		t.Fatalf("首块到达(%v)不早于上游结束(%v)，说明被攒起来了", firstAt, fin)
	}
	if gap := fin.Sub(firstAt); gap < chunkGap {
		t.Fatalf("首块与上游结束只差 %v，小于块间隔 %v，疑似缓冲", gap, chunkGap)
	}

	calls, in, out, _ := rec.snapshot()
	if calls != 1 || in != 5 || out != 4 {
		t.Fatalf("usage 应为 5/4 且只回调一次，实际 calls=%d in=%d out=%d", calls, in, out)
	}
}

// TestMessagesCountTokensSubPath messages 的子路径仍属 Messages 族（不能被当成
// 透明转发），但它不产生 usage，因此不该记账。
func TestMessagesCountTokensSubPath(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
	)
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":17}`)
	})

	var rec usageRecorder
	p.OnUsage = rec.fn

	req := httptest.NewRequest(http.MethodPost, "/v1/llm/anthropic/v1/messages/count_tokens",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	rec2 := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec2, req, Meta{}); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", rec2.Code)
	}
	mu.Lock()
	path := gotPath
	mu.Unlock()
	if path != "/anthropic/v1/messages/count_tokens" {
		t.Fatalf("上游路径应为 /anthropic/v1/messages/count_tokens，实际 %q", path)
	}
	if calls, _, _, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("count_tokens 不该产生 usage，实际回调 %d 次", calls)
	}
}

// TestTransparentPathForwardsOnly 未识别路径（/models 等）只转发不记账，
// 且不因为「没有 model 字段」被拒——这类请求本来就没有 model。
func TestTransparentPathForwardsOnly(t *testing.T) {
	const upstreamResp = `{"object":"list","data":[{"id":"deepseek-chat"}]}`

	var (
		mu       sync.Mutex
		gotPath  string
		gotAuth  string
		gotXAPI  string
		gotMethd string
	)
	p, _ := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		gotPath, gotAuth, gotXAPI, gotMethd = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Method
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamResp)
	})

	var rec usageRecorder
	p.OnUsage = rec.fn

	req := httptest.NewRequest(http.MethodGet, "/v1/llm/models", nil)
	req.Header.Set("Authorization", "Bearer vk-virtual")
	req.Header.Set("X-Api-Key", "vk-virtual")
	rec2 := httptest.NewRecorder()
	if err := p.Serve(context.Background(), rec2, req, Meta{TenantID: "t1"}); err != nil {
		t.Fatalf("透明转发不该因缺少 model 而报错: %v", err)
	}

	if rec2.Code != http.StatusOK || rec2.Body.String() != upstreamResp {
		t.Fatalf("透明转发应原样返回，code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethd != http.MethodGet || gotPath != "/models" {
		t.Fatalf("上游应收到 GET /models，实际 %s %s", gotMethd, gotPath)
	}
	// 认证头照样替换：连 /models 也不能把虚拟 key 递给上游。
	if gotAuth != "Bearer sk-real" || gotXAPI != "" {
		t.Fatalf("非 Messages 请求不该带 x-api-key，且 Authorization 应为真 key，实际 auth=%q x-api-key=%q", gotAuth, gotXAPI)
	}
	if calls, _, _, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("透明转发不该记账，实际回调 %d 次", calls)
	}
}
