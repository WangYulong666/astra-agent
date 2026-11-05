package memory

// Embedding 可插拔：生产用任意 OpenAI 兼容的 /embeddings 端点，
// 没有端点时退化成 hash（仅开发联调用，会打警告）。
//
// 为什么不在这个包里引 SDK：embedding 侧只有一次 HTTP POST，
// 引一个 SDK 等于把「换供应商」的自由度交出去。协议是事实标准，自己发请求更省。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	// embedMaxBatch 单次请求最多带多少条文本。OpenAI 侧上限是 2048，
	// 但一次塞太多会让单请求超时后整批重试，代价被放大；64 是个稳妥的折中。
	embedMaxBatch = 64
	embedTimeout  = 30 * time.Second
	// embedAttempts 含首次在内的总尝试次数。
	embedAttempts = 3
	// defaultHashDim hash embedder 的默认维度。
	defaultHashDim = 256
	// maxDim 单字段向量维度的合理上限，超过基本可以断定是配置错了。
	maxDim = 32768
)

// Embedder 把文本转成向量。Dim 在集合建好之后不可变。
type Embedder interface {
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Name() string
}

// dimSetter 让 openMilvus 在「配置没写维度」时把探测到的维度回填。
// 单独一个接口而不是让 Embedder 带 setter：外部实现不需要关心这件事。
type dimSetter interface {
	setDim(dim int) error
}

// newEmbedder 按配置挑实现，并在维度未知时探测一次。
func newEmbedder(ctx context.Context, cfg Config) (Embedder, error) {
	var e Embedder
	if strings.TrimSpace(cfg.EmbedBaseURL) != "" {
		e = newOpenAIEmbedder(cfg)
	} else {
		// 这条日志必须无条件打出来：hash embedder 只做特征哈希，语义检索基本无效。
		// 不告警的话，用户会以为「记忆搜不准」是产品问题，而不是没配 embedding。
		log.Printf("memory: 使用的是 hash embedder，检索质量仅用于联调（配置 MEMORY_EMBED_BASE_URL 可换成真实 embedding）")
		dim := cfg.Dim
		if dim <= 0 {
			dim = defaultHashDim
		}
		e = newHashEmbedder(dim)
	}

	if e.Dim() > maxDim {
		return nil, fmt.Errorf("memory: embedding 维度 %d 超过上限 %d", e.Dim(), maxDim)
	}
	// 维度探测：集合维度建好就改不了，所以必须以 embedder 的实际输出为准，
	// 不能相信配置里写的数字（模型换了维度就变了）。
	if e.Dim() == 0 {
		vecs, err := e.Embed(ctx, []string{"dim probe"})
		if err != nil {
			return nil, fmt.Errorf("memory: 探测 embedding 维度失败: %w", err)
		}
		if len(vecs) != 1 || len(vecs[0]) == 0 {
			return nil, errors.New("memory: 探测 embedding 维度失败: embedder 返回空向量")
		}
		ds, ok := e.(dimSetter)
		if !ok {
			return nil, errors.New("memory: embedder 未声明维度且不支持回填")
		}
		if err := ds.setDim(len(vecs[0])); err != nil {
			return nil, err
		}
	}
	if e.Dim() <= 0 || e.Dim() > maxDim {
		return nil, fmt.Errorf("memory: embedding 维度 %d 不合法", e.Dim())
	}
	return e, nil
}

// ── OpenAI 兼容 ─────────────────────────────────────────────

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type openAIEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	hc      *http.Client
}

func newOpenAIEmbedder(cfg Config) *openAIEmbedder {
	return &openAIEmbedder{
		baseURL: strings.TrimRight(cfg.EmbedBaseURL, "/"),
		apiKey:  cfg.EmbedAPIKey,
		model:   cfg.EmbedModel,
		dim:     cfg.Dim,
		hc:      &http.Client{Timeout: embedTimeout},
	}
}

func (e *openAIEmbedder) Name() string {
	if e.model == "" {
		return "openai-compatible"
	}
	return "openai-compatible:" + e.model
}

func (e *openAIEmbedder) Dim() int { return e.dim }

func (e *openAIEmbedder) setDim(dim int) error {
	if e.dim != 0 && e.dim != dim {
		return fmt.Errorf("memory: 配置维度 %d 与 embedder 实际维度 %d 不一致", e.dim, dim)
	}
	e.dim = dim
	return nil
}

// Embed 按 embedMaxBatch 切批。切批必须保持顺序：调用方按返回下标对应输入。
func (e *openAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedMaxBatch {
		end := min(start+embedMaxBatch, len(texts))
		vecs, err := e.batch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *openAIEmbedder) batch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts, EncodingFormat: "float"})
	if err != nil {
		return nil, fmt.Errorf("memory: 序列化 embedding 请求: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < embedAttempts; attempt++ {
		if attempt > 0 {
			// 退避 300ms / 600ms：上游限流时立刻重试只会被再拒一次。
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		vecs, retry, err := e.once(ctx, body, len(texts))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("memory: embedding 请求失败（已重试 %d 次）: %w", embedAttempts-1, lastErr)
}

// once 发一次请求。retry 表示「这个错误值得重试」——
// 4xx 是我们的请求有问题（模型名错、key 无效），重试只是浪费配额。
func (e *openAIEmbedder) once(ctx context.Context, body []byte, n int) (vecs [][]float32, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("memory: 构造 embedding 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.hc.Do(req)
	if err != nil {
		// 网络抖动、连接被复用断开都可能瞬时失败，值得重试
		return nil, true, fmt.Errorf("memory: 调用 embedding 端点: %w", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if rerr != nil {
		return nil, true, fmt.Errorf("memory: 读取 embedding 响应: %w", rerr)
	}

	if resp.StatusCode != http.StatusOK {
		retry = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retry, fmt.Errorf("memory: embedding 端点返回 %d: %s", resp.StatusCode, snippet(raw))
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("memory: 解析 embedding 响应: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, false, fmt.Errorf("memory: embedding 端点报错: %s", out.Error.Message)
	}
	if len(out.Data) != n {
		return nil, false, fmt.Errorf("memory: embedding 返回 %d 条，期望 %d 条", len(out.Data), n)
	}

	vecs = make([][]float32, n)
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= n {
			return nil, false, fmt.Errorf("memory: embedding 返回的下标 %d 越界", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, false, errors.New("memory: embedding 返回空向量")
		}
		if e.dim != 0 && len(d.Embedding) != e.dim {
			return nil, false, fmt.Errorf("memory: embedding 维度 %d 与期望 %d 不一致（模型换了？）", len(d.Embedding), e.dim)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, false, fmt.Errorf("memory: embedding 响应缺少第 %d 条", i)
		}
	}
	return vecs, false, nil
}

func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		// 按 rune 截断，别把 UTF-8 切一半
		r := []rune(s)
		if len(r) > max {
			r = r[:max]
		}
		s = string(r) + "…"
	}
	return s
}

// ── hash（仅开发） ───────────────────────────────────────────

// hashEmbedder 是零依赖兜底：对分词结果做特征哈希。
//
// 它只能保证「字面相近的文本向量相近」，没有语义泛化能力。
// 存在的意义是让没配 embedding 的开发环境也能跑通写入/检索链路，
// 绝不能当作生产检索质量。启动时 newEmbedder 会打警告。
type hashEmbedder struct {
	dim int
}

func newHashEmbedder(dim int) *hashEmbedder {
	if dim <= 0 {
		dim = defaultHashDim
	}
	return &hashEmbedder{dim: dim}
}

func (h *hashEmbedder) Name() string { return "hash" }

func (h *hashEmbedder) Dim() int { return h.dim }

func (h *hashEmbedder) setDim(dim int) error {
	if h.dim != 0 && h.dim != dim {
		return fmt.Errorf("memory: hash embedder 维度固定为 %d，不能改成 %d", h.dim, dim)
	}
	h.dim = dim
	return nil
}

func (h *hashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.embedOne(t)
	}
	return out, nil
}

func (h *hashEmbedder) embedOne(text string) []float32 {
	v := make([]float32, h.dim)
	for _, tok := range tokenize(text) {
		sum := fnv1a(tok)
		idx := int(sum % uint64(h.dim))
		// 用高位做符号位：不同 token 撞到同一个桶时有 50% 概率相互抵消，
		// 比一律相加更不容易把无关文本拉近。
		if sum&(1<<63) != 0 {
			v[idx]--
		} else {
			v[idx]++
		}
	}
	if !normalize(v) {
		// 空文本（或全是标点）会得到零向量：L2 距离对零向量没有意义，
		// 也给不出稳定排序。退化成单位基向量，至少保持「同等对待」。
		v[0] = 1
	}
	return v
}

// tokenize 切词：字母/数字成词，其余字符当分隔符。
// 中文没有空格，整段字会被当成一个 token，区分度太低，所以对 CJK 追加 bigram。
func tokenize(s string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		toks = append(toks, strings.ToLower(string(cur)))
		if len(cur) > 1 && hasHan(cur) {
			for i := 0; i+1 < len(cur); i++ {
				toks = append(toks, strings.ToLower(string(cur[i:i+2])))
			}
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return toks
}

func hasHan(rs []rune) bool {
	for _, r := range rs {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func fnv1a(s string) uint64 {
	h := fnv.New64a()
	// strings.Builder 的底层是 []byte，这里直接写，不额外分配
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// normalize 就地做 L2 归一化，返回是否归一化成功（零向量返回 false）。
func normalize(v []float32) bool {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return false
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return true
}
