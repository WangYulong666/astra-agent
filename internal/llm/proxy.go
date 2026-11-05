package llm

// 核心转发：读请求 → 改造成上游能懂的形状 → 转发 → 边转发边抠 usage → 回调。
//
// 三条必须守住的行为，每一条都对应一个线上事故：
//  1. 流式逐块 Flush。攒完再发的话，前端看到的是「卡住十几秒，然后整段蹦出来」，
//     agent 的实时输出等于没有。
//  2. 上游状态码原样透传。把 401 变成 500 会让排查方向从「key 错了」变成
//     「网关挂了」，多花几小时。
//  3. 客户端断开也记账。断的是连接，不是已经生成的 token。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultPathPrefix 网关对外的挂载前缀。
//
// 给 agent 的 base_url 是 "/v1/llm"，agent 拼上 "/v1/chat/completions" 后
// 到网关是 "/v1/llm/v1/chat/completions"，转发给上游前要把这段前缀剥掉。
const DefaultPathPrefix = "/v1/llm"

var (
	// ErrBadRequest 请求体不可用（不是 JSON、缺 model）。
	ErrBadRequest = errors.New("llm: 请求不合法")
	// ErrUpstream 连不上上游/上游响应读失败。调用方应回 502，而不是 500——
	// 500 会让人以为是我们自己的 bug。
	ErrUpstream = errors.New("llm: 上游请求失败")
)

// Meta 一次调用的归属信息，由调用方从虚拟 key 的 claims 里取。
//
// 刻意不包含 APIKey：key 只属于 provider，任何来自请求的东西都不能影响它。
type Meta struct {
	TenantID  string
	UserID    string
	SessionID string
	Provider  string // 空表示按 model 前缀推断
	Model     string // 请求体里没有 model 时的兜底
}

// Proxy 转发器。构造后除 OnUsage 外只读；并发安全的前提见 Gateway.HandleWith
// （每次请求用浅拷贝挂自己的回调，不改共享实例）。
type Proxy struct {
	Registry   *Registry
	Pricing    *Pricing
	Client     *http.Client
	PathPrefix string

	// OnUsage 拿到 usage 后回调。model 是上游真实回报的模型名（拿不到时用请求里的）。
	// 允许为 nil：单测里只关心转发时可以不挂。
	OnUsage func(model string, in, out int64)
}

// usage 上游回报的 token 用量。字段名按 OpenAI 兼容协议。
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// usageEnvelope 非流式响应与流式末帧共用的外壳，只取我们关心的两个字段。
type usageEnvelope struct {
	Model string `json:"model"`
	Usage *usage `json:"usage"`
}

// usageTracker 一次调用的 usage 累积器。
//
// 抽成接口是因为两种协议拿 usage 的节奏不同：OpenAI 是最后一帧一个完整对象，
// Messages 是首帧给 input、尾帧给累计 output，必须攒到流结束才能合并。
// 但「逐行喂、结束时取一次结果」的外层循环是共用的，所以差异收进实现里，
// pipeStream 不用按协议写分支。
type usageTracker interface {
	// feed 吃一行原始 SSE（可能带 `event:` / `data:` 前缀，也可能是空行）
	feed(line []byte)
	// modelName 上游回报的模型名，拿不到返回空串
	modelName() string
	// result 最终用量；ok=false 表示整轮没见到任何 usage，不该记账
	result() (in, out int64, ok bool)
}

// newUsageTracker 按协议族给累积器。透明转发不解析 usage，返回 nil。
func newUsageTracker(proto Protocol) usageTracker {
	switch proto {
	case ProtocolOpenAI:
		return &openAIUsage{}
	case ProtocolMessages:
		return &messagesUsage{}
	}
	return nil
}

// openAIUsage OpenAI 兼容协议的累积器：usage 在末帧一次性给全。
type openAIUsage struct {
	u    *usage
	name string
}

func (t *openAIUsage) feed(line []byte) {
	u, m := usageFromSSELine(line)
	if u != nil {
		t.u = u
	}
	if m != "" {
		t.name = m
	}
}

func (t *openAIUsage) modelName() string { return t.name }

func (t *openAIUsage) result() (int64, int64, bool) {
	if t.u == nil {
		return 0, 0, false
	}
	return t.u.PromptTokens, t.u.CompletionTokens, true
}

// Serve 转发一次调用。
//
// 返回 error 只在「还没往 w 写任何字节」时发生（调用方据此决定回 502/400）；
// 一旦开始写响应体，状态码与响应体就归上游所有，我们只负责搬字节。
func (p *Proxy) Serve(ctx context.Context, w http.ResponseWriter, r *http.Request, meta Meta) error {
	body, err := readBody(r)
	if err != nil {
		return fmt.Errorf("%w: 读取请求体: %v", ErrBadRequest, err)
	}

	// 路径既是转发目标也是协议依据：dsh 的 baseURL 配成 {网关}/v1/llm/anthropic，
	// 它拼出 /v1/llm/anthropic/v1/messages，剥掉 /v1/llm 后正是上游要的
	// /anthropic/v1/messages。协议在这里判一次，后面所有分支都用它。
	target := p.targetPath(r)
	proto := detectProtocol(target)

	outBody, model, streaming := prepareBody(proto, body)
	if model == "" {
		model = meta.Model
	}
	// 透明转发没有 model 可查（GET /models 之类），按 provider 兜底即可；
	// 两种对话协议缺 model 就一定记不了账，直接拒。
	if proto != ProtocolTransparent && strings.TrimSpace(model) == "" {
		return fmt.Errorf("%w: 缺少 model 字段", ErrBadRequest)
	}

	up, err := p.Registry.Resolve(meta.Provider, model)
	if err != nil {
		return err
	}

	// 上游 context 与客户端解绑：客户端断开后这一轮仍然要跑完并取到 usage。
	// 断连不等于没花钱——上游已经把输出 token 生成出来了，账必须在。
	// 同时保留 ctx 的 value（链路追踪之类的上下文不丢）。
	upCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	req, err := http.NewRequestWithContext(upCtx, r.Method, joinURL(up.BaseURL, target), bytes.NewReader(outBody))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	copyRequestHeader(req.Header, r.Header)
	// 客户端带来的 Authorization 是虚拟 key，必须换成真 key；
	// 其它任何形式的 key 头也要删掉，避免 agent 自带 key 绕过记账。
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Del("X-Api-Key")
	req.Header.Del("Api-Key")
	// Messages 额外双写 x-api-key：Anthropic 原生只认 x-api-key，OpenAI 兼容层
	// 只认 Authorization，不同上游实现取向不一。两个都带没有副作用（无关的那个
	// 被忽略），只写一个则可能在某一类上游上直接 401。
	if proto == ProtocolMessages {
		req.Header.Set("x-api-key", up.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(outBody))

	resp, err := p.client().Do(req)
	if err != nil {
		// 客户端自己断开时不值得记一条「上游失败」——它不是上游的问题。
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	// 只有 200 的流式响应才走边转发边解析；4xx/5xx 一律按整体透传，
	// 这样上游的错误体（含 request id、具体原因）能一字不改地到调用方眼前。
	// 透明转发同样走流式通道：它拿不到 tracker（不记账），但流式语义必须保住——
	// 「透明」的意思是客户端看到什么由上游决定，不是我们替它攒着。
	if streaming && resp.StatusCode == http.StatusOK {
		return p.pipeStream(ctx, w, resp, model, proto)
	}
	return p.pipeWhole(w, resp, model, proto)
}

// ── 转发 ─────────────────────────────────────────────────────

// pipeWhole 整体透传：非流式，以及一切错误响应。
func (p *Proxy) pipeWhole(w http.ResponseWriter, resp *http.Response, model string, proto Protocol) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// 还没写状态码，可以如实报 502。
		return fmt.Errorf("%w: 读取上游响应: %v", ErrUpstream, err)
	}
	hdr := w.Header()
	copyResponseHeader(hdr, resp.Header)
	// 长度已知就写死，省得客户端一直等到连接关闭才知道结束了。
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	// 错误响应里不会有 usage；这里只处理 2xx，避免把上游的错误体当成功解析。
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if m, in, out, ok := usageFromBody(proto, body); ok {
			p.record(firstNonEmpty(m, model), in, out)
		}
	}
	return nil
}

// usageFromBody 从非流式响应体里取 usage。按协议族分派，形状差异都在各自的
// 解析函数里，这里只做选择——加第三种协议时只多一个 case。
func usageFromBody(proto Protocol, body []byte) (model string, in, out int64, ok bool) {
	switch proto {
	case ProtocolOpenAI:
		var env usageEnvelope
		if json.Unmarshal(body, &env) != nil || env.Usage == nil {
			return "", 0, 0, false
		}
		return env.Model, env.Usage.PromptTokens, env.Usage.CompletionTokens, true
	case ProtocolMessages:
		return usageFromMessagesBody(body)
	}
	return "", 0, 0, false // 透明转发：不解析，也就没有账
}

// pipeStream 逐行转发 SSE，同时从 data 帧里抠 usage。
//
// 按行而不是按块：SSE 的帧边界是 "\n\n"，按块读可能把一帧切成两半，
// 半帧 JSON 解析必然失败，usage 就丢了。
func (p *Proxy) pipeStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, model string, proto Protocol) error {
	fl, _ := w.(http.Flusher)
	hdr := w.Header()
	copyResponseHeader(hdr, resp.Header)
	// 流式响应没有确定长度；同时关掉沿途代理的缓冲，否则逐块 Flush 白做。
	hdr.Del("Content-Length")
	hdr.Set("Cache-Control", "no-cache, no-transform")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flush(fl)

	br := bufio.NewReaderSize(resp.Body, 32<<10)
	// clientGone 一旦置位就只读不写：继续把上游读完，才能拿到最后一帧的 usage。
	// 这是「断开也记账」的全部秘密——不是估一个数，而是把账真的收完。
	clientGone := false
	tracker := newUsageTracker(proto)

	for {
		line, rerr := br.ReadBytes('\n')
		// 先看连接还在不在再写：客户端已断开时写会阻塞在 TCP 缓冲上，
		// 那会让「继续读上游拿 usage」这件事一起卡住。
		if !clientGone && ctx.Err() != nil {
			clientGone = true
		}
		if len(line) > 0 {
			if !clientGone {
				if _, werr := w.Write(line); werr != nil {
					clientGone = true
				} else {
					flush(fl)
				}
			}
			if tracker != nil {
				tracker.feed(line)
			}
		}
		if rerr != nil {
			// io.EOF 是正常结束；其它错误意味着上游中途断了。
			// 两种情况都已经收到的 token 照样记账（下面统一回调）。
			break
		}
	}

	if tracker != nil {
		if in, out, ok := tracker.result(); ok {
			p.record(firstNonEmpty(tracker.modelName(), model), in, out)
		}
	}
	return nil
}

func (p *Proxy) record(model string, in, out int64) {
	if p.OnUsage == nil {
		return
	}
	p.OnUsage(model, in, out)
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	// 不设整体 Timeout：流式响应可能持续几分钟，Client.Timeout 会把长回答掐断。
	// 只限制「等响应头」的时间，这样上游卡死时不会永远挂着。
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 120 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

func (p *Proxy) prefix() string {
	if v := strings.TrimSpace(p.PathPrefix); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultPathPrefix
}

// targetPath 把网关路径还原成上游路径：/v1/llm/v1/chat/completions → /v1/chat/completions。
func (p *Proxy) targetPath(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if pre := p.prefix(); pre != "" && strings.HasPrefix(path, pre) {
		path = strings.TrimPrefix(path, pre)
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	return path
}

// ── 请求/响应整形 ─────────────────────────────────────────────

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	// 上限 16MB：正常 chat 请求远小于这个数，设上限是防止有人拿网关当文件上传通道。
	return io.ReadAll(io.LimitReader(r.Body, 16<<20))
}

// prepareBody 解析请求体，并**只在 OpenAI 协议**上补 stream_options.include_usage。
//
// 为什么必须补：DeepSeek（以及多数 OpenAI 兼容实现）默认不在流式末帧返回 usage，
// 只有显式要求 include_usage 才给。拿不到 usage 就没法记账，而记账是网关存在的理由。
// 已经是 true 时原样透传，避免无谓地重写 body。
//
// 为什么必须限定协议：stream_options 是 OpenAI 专有字段，Anthropic Messages
// 端点不认识它，很可能直接回 400；而 Messages 的 usage 本来就由 message_start /
// message_delta 主动给出，压根不需要这个开关。无条件注入等于把一条能用的链路弄坏。
func prepareBody(proto Protocol, body []byte) (out []byte, model string, streaming bool) {
	m := decodeObject(body)
	if m == nil {
		// 不是 JSON 对象：拿不到 model，也就没法选 provider、没法记账。
		// 由调用方回 400 —— 这种请求即使转给上游也只会拿到 400/422。
		return body, "", false
	}
	if s, ok := m["model"].(string); ok {
		model = s
	}
	if b, ok := m["stream"].(bool); ok && b {
		streaming = true
	}
	if !streaming || proto != ProtocolOpenAI {
		return body, model, streaming
	}

	opts, _ := m["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if v, ok := opts["include_usage"].(bool); ok && v {
		return body, model, true
	}
	opts["include_usage"] = true
	m["stream_options"] = opts

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // prompt 里常有 < > &，转义只会让上游收到的内容变样
	if err := enc.Encode(m); err != nil {
		return body, model, true
	}
	return buf.Bytes(), model, true
}

func decodeObject(body []byte) map[string]any {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// UseNumber：默认会把整数解成 float64，max_tokens 这类值重新编码后
	// 可能变成 4.096e+03 这种形式，虽然多数上游能接受，但没必要冒这个险。
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	return m
}

// usageFromSSELine 从一行 SSE 里取 usage。
//
// 行形如 `data: {"id":"...","usage":{"prompt_tokens":10,...}}`。
// 中间帧的 usage 是 null，末尾帧才是数字，所以只看非 nil。
func usageFromSSELine(line []byte) (*usage, string) {
	payload, ok := sseData(line)
	if !ok {
		return nil, ""
	}
	var env usageEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		// 上游偶尔会插心跳/注释帧，解不出来是正常的，不要因此中断转发。
		return nil, ""
	}
	if env.Usage == nil {
		return nil, ""
	}
	return env.Usage, env.Model
}

// sseData 取一行 SSE 的 data 载荷。`event:` 行、空行、注释行都返回 false。
//
// 两种协议的帧外壳是同一套（event:/data:/空行分隔），只有 data 里的 JSON 不同，
// 所以「剥外壳」这一步共用，协议差异留在各自的解析函数里。
func sseData(line []byte) (string, bool) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return "", false
	}
	s = strings.TrimSpace(s[len("data:"):])
	if s == "" || s == "[DONE]" {
		return "", false
	}
	return s, true
}

// ── header 处理 ──────────────────────────────────────────────

// hopHeaders 逐跳头，不能跨代理转发（RFC 7230 §6.1）。
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyRequestHeader(dst, src http.Header) {
	for k, vs := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		// 不让客户端指定 Accept-Encoding：上游回 gzip 的 SSE 没法按行解析，
		// 而 Go 的 Transport 只在「自己加的」头下才会透明解压。
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeader(dst, src http.Header) {
	for k, vs := range src {
		if isHopHeader(k) {
			continue
		}
		dst.Del(k)
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isHopHeader(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func flush(fl http.Flusher) {
	if fl != nil {
		fl.Flush()
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
