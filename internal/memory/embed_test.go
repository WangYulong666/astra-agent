package memory

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHashEmbedderDimAndNorm(t *testing.T) {
	e := newHashEmbedder(64)
	if e.Dim() != 64 {
		t.Fatalf("Dim() = %d, want 64", e.Dim())
	}
	if e.Name() != "hash" {
		t.Fatalf("Name() = %q, want hash", e.Name())
	}
	vecs, err := e.Embed(context.Background(), []string{"用户想要一份季报摘要", "hello world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("返回 %d 条向量，want 2", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 64 {
			t.Fatalf("第 %d 条维度 %d，want 64", i, len(v))
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(sum)-1) > 1e-4 {
			t.Fatalf("第 %d 条模长 %.6f，期望归一化到 1", i, math.Sqrt(sum))
		}
	}
}

func TestHashEmbedderStable(t *testing.T) {
	e := newHashEmbedder(32)
	a, err := e.Embed(context.Background(), []string{"同一个文案"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(context.Background(), []string{"同一个文案"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("同一文案两次结果不同（第 %d 维）", i)
		}
	}

	// 不同文本应当产生不同向量，否则检索等于随机
	c, err := e.Embed(context.Background(), []string{"完全无关的另一句"})
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range a[0] {
		if a[0][i] != c[0][i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("不同文本得到了相同向量")
	}
}

func TestHashEmbedderEmptyText(t *testing.T) {
	e := newHashEmbedder(8)
	vecs, err := e.Embed(context.Background(), []string{"!!!"})
	if err != nil {
		t.Fatal(err)
	}
	// 全标点会被分词成空集：不能返回零向量（L2 距离对零向量没有意义）
	var sum float64
	for _, x := range vecs[0] {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		t.Fatal("空 token 文本返回了零向量")
	}
}

// ── OpenAI 兼容 ─────────────────────────────────────────────

func fakeOpenAI(t *testing.T, dim int, calls *int, sizes *[]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.URL.Path != "/embeddings" {
			t.Errorf("路径 = %s, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var req embedRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体不是 JSON: %v", err)
			return
		}
		if req.Model != "text-embedding-3-small" {
			t.Errorf("model = %q", req.Model)
		}
		*sizes = append(*sizes, len(req.Input))

		data := make([]map[string]any, 0, len(req.Input))
		for i := range req.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = 0.1
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func TestOpenAIEmbedderBatchAndProbe(t *testing.T) {
	var calls int
	var sizes []int
	srv := fakeOpenAI(t, 4, &calls, &sizes)
	defer srv.Close()

	// Dim 为 0：必须先探测维度，探测也算一次真实请求
	e, err := newEmbedder(context.Background(), Config{
		EmbedBaseURL: srv.URL,
		EmbedAPIKey:  "test-key",
		EmbedModel:   "text-embedding-3-small",
	})
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	if e.Dim() != 4 {
		t.Fatalf("探测维度 = %d, want 4", e.Dim())
	}
	if !strings.Contains(e.Name(), "text-embedding-3-small") {
		t.Fatalf("Name() = %q，应包含模型名", e.Name())
	}
	if calls != 1 {
		t.Fatalf("探测应只发一次请求，实际 %d", calls)
	}

	// 65 条必须切成 64 + 1 两批
	texts := make([]string, 65)
	for i := range texts {
		texts[i] = "t"
	}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 65 {
		t.Fatalf("返回 %d 条，want 65", len(vecs))
	}
	if calls != 3 {
		t.Fatalf("探测 1 次 + 批量 2 次 = 3，实际 %d", calls)
	}
	if len(sizes) != 3 || sizes[0] != 1 || sizes[1] != embedMaxBatch || sizes[2] != 1 {
		t.Fatalf("每批条数 = %v，want [1 64 1]", sizes)
	}
	for i, v := range vecs {
		if len(v) != 4 {
			t.Fatalf("第 %d 条维度 %d", i, len(v))
		}
	}
}

func TestOpenAIEmbedderServerErrorPropagates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newOpenAIEmbedder(Config{EmbedBaseURL: srv.URL, EmbedModel: "m", Dim: 8})
	if _, err := e.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("5xx 应当返回错误")
	}
	if calls != embedAttempts {
		t.Fatalf("5xx 应重试到 %d 次，实际 %d", embedAttempts, calls)
	}

	// 4xx 是我们自己的问题（模型名/密钥），重试只会浪费配额
	calls = 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer bad.Close()
	e2 := newOpenAIEmbedder(Config{EmbedBaseURL: bad.URL, EmbedModel: "m", Dim: 8})
	if _, err := e2.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("4xx 应当返回错误")
	}
	if calls != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", calls)
	}
}

func TestOpenAIEmbedderDimMismatch(t *testing.T) {
	var calls int
	var sizes []int
	srv := fakeOpenAI(t, 3, &calls, &sizes)
	defer srv.Close()

	e := newOpenAIEmbedder(Config{
		EmbedBaseURL: srv.URL,
		EmbedAPIKey:  "test-key",
		EmbedModel:   "text-embedding-3-small",
		Dim:          8,
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "维度") {
		t.Fatalf("维度不符应报错，实际 %v", err)
	}
}

func TestHashEmbedderWarningIsLogged(t *testing.T) {
	// 没有 EmbedBaseURL 时必须走 hash 且返回可用向量（警告日志由 log 包输出，
	// 单测里只能验证行为；文案见 embed.go 与汇报）。
	e, err := newEmbedder(context.Background(), Config{Dim: 0})
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	if e.Name() != "hash" {
		t.Fatalf("Name() = %q, want hash", e.Name())
	}
	if e.Dim() != defaultHashDim {
		t.Fatalf("Dim() = %d, want %d", e.Dim(), defaultHashDim)
	}
}
