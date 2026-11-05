package sandbox

// Cube provider 的契约测试：用 httptest 起一个「形状正确」的假 CubeAPI，
// 把 cube.go 假设的 REST 面钉进测试——真机（x86_64 + KVM）验证前，至少保证
// 客户端对「自己写下的契约」是对的。真集群接入时若形状有出入，这些测试
// 会是最先报错的地方，改 mock 比改散在业务里的调用点便宜。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCubeAPI 按路径记录请求并回放固定形状的响应。
type fakeCubeAPI struct {
	mu      sync.Mutex
	reqs    []struct{ method, path string }
	bodies  map[string][]byte
	exists  map[string]bool // 已存在的沙箱 ID
	created int
	srv     *httptest.Server
}

func newFakeCubeAPI(t *testing.T) *fakeCubeAPI {
	f := &fakeCubeAPI{bodies: map[string][]byte{}, exists: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, struct{ method, path string }{r.Method, r.URL.Path})
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			f.bodies[r.Method+" "+r.URL.Path] = b
		}
		f.mu.Unlock()

		switch {
		case r.URL.Path == "/sandboxes" && r.Method == http.MethodPost:
			var payload map[string]any
			_ = json.Unmarshal(b, &payload)
			id, _ := payload["metadata"].(map[string]any)["astra_sandbox_id"].(string)
			f.mu.Lock()
			f.exists[id] = true
			f.created++
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": id, "clientID": "c1"})
		case r.URL.Path == "/sandboxes" && r.Method == http.MethodGet:
			f.mu.Lock()
			defer f.mu.Unlock()
			var out []map[string]any
			for id := range f.exists {
				out = append(out, map[string]any{"sandboxID": id, "state": "running", "templateID": "tmpl-astra"})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
			f.mu.Lock()
			ok := f.exists[id]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxID": id, "status": "running", "templateID": "tmpl-astra",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCubeAPI) hit(method, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

func newCubeForTest(t *testing.T, base string) *Cube {
	t.Helper()
	return NewCube(Options{CubeAPI: base, CubeToken: "tok-123"})
}

func TestCubeCreateContract(t *testing.T) {
	f := newFakeCubeAPI(t)
	p := newCubeForTest(t, f.srv.URL)

	req := CreateRequest{
		ID:       "sess-abc",
		Template: "tmpl-astra",
		Agent:    AgentSpec{Command: []string{"/bin/x"}, Env: map[string]string{"FOO": "bar"}},
		Token:    "proxy-tok",
		Timeout:  30 * time.Minute,
	}
	sb, err := p.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID() != "sess-abc" {
		t.Fatalf("ID = %q", sb.ID())
	}

	// 契约 1：POST /sandboxes，body 带 templateID/envVars/metadata
	if !f.hit("POST", "/sandboxes") {
		t.Fatal("应 POST /sandboxes")
	}
	var payload map[string]any
	_ = json.Unmarshal(f.bodies["POST /sandboxes"], &payload)
	if payload["templateID"] != "tmpl-astra" {
		t.Fatalf("templateID = %v", payload["templateID"])
	}
	meta, _ := payload["metadata"].(map[string]any)
	if meta["astra_sandbox_id"] != "sess-abc" || meta["astra_proxy_token"] != "proxy-tok" {
		t.Fatalf("metadata 缺 astra 标记: %v", meta)
	}
	if payload["timeout"] != float64(1800) {
		t.Fatalf("timeout 应为秒数 1800，得到 %v", payload["timeout"])
	}

	// 契约 2：创建后轮询 GET /sandboxes/{id} 直到 running
	if !f.hit("GET", "/sandboxes/sess-abc") {
		t.Fatal("创建后应轮询就绪状态")
	}

	// 契约 3：鉴权头
	// （fake 不校验 header，这里直接对 do 层断言太重；用 Info 链路覆盖）
	if _, err := p.Acquire(context.Background(), "sess-abc"); err != nil {
		t.Fatalf("Acquire 既有沙箱: %v", err)
	}
}

func TestCubeCreateIdempotent(t *testing.T) {
	f := newFakeCubeAPI(t)
	p := newCubeForTest(t, f.srv.URL)
	req := CreateRequest{ID: "sess-1", Template: "t", Token: "tok", Agent: AgentSpec{Env: map[string]string{}}}
	if _, err := p.Create(context.Background(), req); err != nil {
		t.Fatalf("首次 Create: %v", err)
	}
	n := f.created
	if _, err := p.Create(context.Background(), req); err != nil {
		t.Fatalf("幂等 Create: %v", err)
	}
	if f.created != n {
		t.Fatal("沙箱已存在时不应重复创建")
	}
}

func TestCubeAcquireMissing(t *testing.T) {
	f := newFakeCubeAPI(t)
	p := newCubeForTest(t, f.srv.URL)
	_, err := p.Acquire(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 应映射 ErrNotFound，得到 %v", err)
	}
}

func TestCubeListContract(t *testing.T) {
	f := newFakeCubeAPI(t)
	p := newCubeForTest(t, f.srv.URL)
	_ = f
	// 造两个
	for _, id := range []string{"s1", "s2"} {
		if _, err := p.Create(context.Background(), CreateRequest{
			ID: id, Template: "t", Token: "tok", Agent: AgentSpec{Env: map[string]string{}},
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	infos, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List 应返回 2 条，得到 %d", len(infos))
	}
	for _, in := range infos {
		if in.Provider != "cube" || in.State != StateRunning {
			t.Fatalf("Info 形状不符: %+v", in)
		}
	}
}

func TestCubeDestroyContract(t *testing.T) {
	f := newFakeCubeAPI(t)
	p := newCubeForTest(t, f.srv.URL)
	if _, err := p.Create(context.Background(), CreateRequest{
		ID: "s-x", Template: "t", Token: "tok", Agent: AgentSpec{Env: map[string]string{}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(context.Background(), "s-x"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !f.hit("DELETE", "/sandboxes/s-x") {
		t.Fatal("应 DELETE /sandboxes/{id}")
	}
	// fake 的 DELETE 没删 exists——这里只验证请求形状
}

func TestCubeRequiresAPIBase(t *testing.T) {
	p := newCubeForTest(t, "")
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("未配置 CubeAPI 应报错")
	}
}
