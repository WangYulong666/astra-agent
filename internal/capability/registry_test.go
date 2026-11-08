package capability

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeSources 是可被并发 Collect 的假数据源。Collect 返回防御性拷贝，
// 这样测试里改 spec 不会反过来影响注册表缓存，也不会制造额外竞态。
type fakeSources struct {
	mu      sync.Mutex
	specs   []Spec
	failErr error
	calls   int
}

func (f *fakeSources) Collect(_ context.Context) ([]Spec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failErr != nil {
		return nil, f.failErr
	}
	out := make([]Spec, len(f.specs))
	for i := range f.specs {
		out[i] = f.specs[i]
		out[i].RequiredEnv = append([]string(nil), f.specs[i].RequiredEnv...)
		out[i].Requires = append([]string(nil), f.specs[i].Requires...)
	}
	return out, nil
}

func (f *fakeSources) set(specs []Spec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]Spec, len(specs))
	for i := range specs {
		cp[i] = specs[i]
		cp[i].RequiredEnv = append([]string(nil), specs[i].RequiredEnv...)
		cp[i].Requires = append([]string(nil), specs[i].Requires...)
	}
	f.specs = cp
}

func (f *fakeSources) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = err
}

func (f *fakeSources) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func baseSpecs() []Spec {
	return []Spec{
		{
			Name: "GET /v1/memory/search", Kind: "api", Description: "检索记忆",
			Enabled: true, Endpoint: "GET /v1/memory/search", AuthAction: "memory:read",
			RequiredEnv: []string{"MILVUS_ADDR"}, Requires: []string{"memory"},
			Source: "openapi", Body: "openapi-body",
		},
		{
			Name: "astra-cli memory search", Kind: "cli", Description: "命令行检索记忆",
			Enabled: true, Endpoint: "astra-cli memory search \"...\"",
			Requires: []string{"memory"}, Source: "astra-cli", Body: "cli-body",
		},
		{
			Name: "skill-demo", Kind: "skill", Description: "示例 skill",
			Enabled: false, RequiredEnv: []string{"MILVUS_ADDR"},
			Requires: []string{"memory"}, Source: "builtin", Body: "SKILL.md 正文",
		},
	}
}

func byName(items []Item) map[string]Item {
	m := make(map[string]Item, len(items))
	for _, it := range items {
		m[it.Name] = it
	}
	return m
}

func TestOpenValidation(t *testing.T) {
	if _, err := Open(nil); err == nil {
		t.Fatal("src == nil 时 Open 必须返回错误")
	}
	want := errors.New("collect boom")
	if _, err := Open(&fakeSources{failErr: want}); !errors.Is(err, want) {
		t.Fatalf("首次 Collect 失败应原样返回错误，实际 %v", err)
	}
}

func TestListDetailLevels(t *testing.T) {
	r, err := Open(&fakeSources{specs: baseSpecs()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	shallow, err := r.List(ctx, false)
	if err != nil {
		t.Fatalf("List(false): %v", err)
	}
	full, err := r.List(ctx, true)
	if err != nil {
		t.Fatalf("List(true): %v", err)
	}
	if len(shallow) != len(full) || len(full) != len(baseSpecs()) {
		t.Fatalf("两层条目数应一致且等于 spec 数：shallow=%d full=%d", len(shallow), len(full))
	}

	s := byName(shallow)
	f := byName(full)
	for i := range shallow {
		if shallow[i].Name != full[i].Name {
			t.Fatalf("两层顺序不一致：shallow[%d]=%q full[%d]=%q", i, shallow[i].Name, i, full[i].Name)
		}
	}

	// detail=false：第二层字段一律清空，只留第一层。
	for _, it := range shallow {
		if it.Endpoint != "" || it.AuthAction != "" || it.Body != "" ||
			it.RequiredEnv != nil || it.Requires != nil {
			t.Errorf("detail=false 泄漏了详细字段：%+v", it)
		}
		if it.Name == "" || it.Kind == "" || it.Description == "" || it.Source == "" {
			t.Errorf("detail=false 丢了第一层字段：%+v", it)
		}
	}
	if !s["skill-demo"].Enabled && s["skill-demo"].Unavailable == "" {
		t.Error("Enabled=false 却没写 Unavailable 原因")
	}
	if f["GET /v1/memory/search"].Enabled != true {
		t.Error("detail=false 弄错了 Enabled")
	}

	// detail=true：全量字段都在。
	api := f["GET /v1/memory/search"]
	if api.Endpoint != "GET /v1/memory/search" || api.AuthAction != "memory:read" ||
		api.Body != "openapi-body" || len(api.RequiredEnv) != 1 || len(api.Requires) != 1 {
		t.Errorf("detail=true 应返回全量字段：%+v", api)
	}
	if f["skill-demo"].Body != "SKILL.md 正文" {
		t.Error("detail=true 应包含 skill 的 Body")
	}

	// 返回的切片必须是拷贝：改它不能污染缓存，也不能被后续 Refresh 串改。
	shallow[0].Name = "tampered"
	full[0].Requires[0] = "tampered"
	again, _ := r.List(ctx, true)
	g := byName(again)
	if g["GET /v1/memory/search"].Name != "GET /v1/memory/search" ||
		g["GET /v1/memory/search"].Requires[0] != "memory" {
		t.Error("List 返回值与缓存共享了底层数组（防御性拷贝失效）")
	}
}

func TestGet(t *testing.T) {
	r, err := Open(&fakeSources{specs: baseSpecs()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	got, err := r.Get(ctx, "GET /v1/memory/search")
	if err != nil {
		t.Fatalf("Get 已存在项: %v", err)
	}
	if got.Body != "openapi-body" || got.Endpoint != "GET /v1/memory/search" ||
		len(got.RequiredEnv) != 1 || len(got.Requires) != 1 {
		t.Errorf("Get 应返回单项全文，实际 %+v", got)
	}

	// 精确匹配：前缀命中不算，模糊不算。
	for _, name := range []string{"GET /v1/memory", "memory", "get /v1/memory/search", "", "nope"} {
		if _, err := r.Get(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) 应返回 ErrNotFound，实际 %v", name, err)
		}
	}

	// Get 的返回值同样不能与缓存共享。
	got.Requires[0] = "tampered"
	again, _ := r.Get(ctx, "GET /v1/memory/search")
	if again.Requires[0] != "memory" {
		t.Error("Get 返回值与缓存共享了底层数组")
	}
}

func TestRefresh(t *testing.T) {
	f := &fakeSources{specs: baseSpecs()}
	r, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	before, _ := r.List(ctx, true)
	if _, err := r.Get(ctx, "brand-new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("初始快照不应有 brand-new")
	}

	next := baseSpecs()
	next[0].Body = "changed-body"
	next = append(next, Spec{Name: "brand-new", Kind: "cli", Description: "新增命令", Enabled: true, Source: "astra-cli"})
	f.set(next)
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	after, _ := r.List(ctx, true)
	a := byName(after)
	if a["GET /v1/memory/search"].Body != "changed-body" {
		t.Error("Refresh 后应看到更新后的 Body")
	}
	if _, ok := a["brand-new"]; !ok {
		t.Error("Refresh 后应出现新增条目")
	}
	if len(after) != len(before)+1 {
		t.Errorf("Refresh 后条目数应为 %d，实际 %d", len(before)+1, len(after))
	}
	// 之前 List 拷出的快照是独立副本，不受 Refresh 影响。
	if byName(before)["GET /v1/memory/search"].Body != "openapi-body" {
		t.Error("Refresh 篡改了此前返回的快照")
	}
	if f.callCount() != 2 {
		t.Errorf("Open + Refresh 应恰好 Collect 两次，实际 %d", f.callCount())
	}

	// Refresh 失败：返回错误且保留旧快照，不能把缓存清成空。
	boom := errors.New("refresh boom")
	f.setFail(boom)
	if err := r.Refresh(ctx); !errors.Is(err, boom) {
		t.Fatalf("Refresh 失败应返回错误，实际 %v", err)
	}
	stillThere, _ := r.List(ctx, true)
	if _, ok := byName(stillThere)["brand-new"]; !ok {
		t.Error("Refresh 失败不应清空已有快照")
	}
}

func TestSortStable(t *testing.T) {
	specs := []Spec{
		{Name: "z-skill", Kind: "skill", Enabled: true},
		{Name: "m-api", Kind: "api", Enabled: true},
		{Name: "a-cli", Kind: "cli", Enabled: true},
		{Name: "a-api", Kind: "api", Enabled: true},
		{Name: "z-cli", Kind: "cli", Enabled: true},
		{Name: "a-skill", Kind: "skill", Enabled: true},
		{Name: "m-api-2", Kind: "api", Enabled: true},
	}
	r, err := Open(&fakeSources{specs: specs})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	wantSorted := func(items []Item) bool {
		return sort.SliceIsSorted(items, func(i, j int) bool {
			if items[i].Kind != items[j].Kind {
				return items[i].Kind < items[j].Kind
			}
			return items[i].Name < items[j].Name
		})
	}

	first, _ := r.List(ctx, true)
	if !wantSorted(first) {
		t.Fatalf("未按 Kind 再 Name 排序：%v", names(first))
	}
	// 多次调用顺序完全一致（输入是乱序的，结果稳定）。
	for i := 0; i < 5; i++ {
		got, _ := r.List(ctx, true)
		if strings.Join(names(got), "|") != strings.Join(names(first), "|") {
			t.Fatalf("第 %d 次 List 顺序漂移：%v", i, names(got))
		}
	}
	// detail=false 与 detail=true 顺序一致。
	shallow, _ := r.List(ctx, false)
	if strings.Join(names(shallow), "|") != strings.Join(names(first), "|") {
		t.Fatal("detail 两层顺序不一致")
	}
}

func names(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Kind + "/" + it.Name
	}
	return out
}

func TestUnavailable(t *testing.T) {
	specs := []Spec{
		{Name: "enabled-dep", Kind: "api", Enabled: true},
		{Name: "disabled-dep", Kind: "api", Enabled: false},
		{Name: "needs-env", Kind: "api", Enabled: false, RequiredEnv: []string{"MILVUS_ADDR"}},
		{Name: "needs-multi-env", Kind: "api", Enabled: false,
			RequiredEnv: []string{"LANGFUSE_HOST", "LANGFUSE_PUBLIC_KEY", "LANGFUSE_SECRET_KEY"}},
		{Name: "needs-req", Kind: "skill", Enabled: false, Requires: []string{"astra-cli", "browser"}},
		{Name: "disabled-no-hint", Kind: "cli", Enabled: false},
		{Name: "ok", Kind: "cli", Enabled: true},
		{Name: "dep-disabled", Kind: "cli", Enabled: true, Requires: []string{"disabled-dep", "enabled-dep"}},
	}
	r, err := Open(&fakeSources{specs: specs})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	items, _ := r.List(context.Background(), true)
	m := byName(items)

	if got := m["needs-env"].Unavailable; !strings.Contains(got, "需要 MILVUS_ADDR") {
		t.Errorf("应写清缺少的环境变量，实际 %q", got)
	}
	if got := m["needs-multi-env"].Unavailable; !strings.Contains(got, "LANGFUSE_HOST") ||
		!strings.Contains(got, "LANGFUSE_SECRET_KEY") {
		t.Errorf("多个 RequiredEnv 都应列出，实际 %q", got)
	}
	if got := m["needs-req"].Unavailable; !strings.Contains(got, "依赖未就绪") ||
		!strings.Contains(got, "astra-cli") {
		t.Errorf("无 env 线索时应写明 Requires，实际 %q", got)
	}
	if got := m["disabled-no-hint"].Unavailable; got == "" {
		t.Error("Enabled=false 且无线索时也要给默认原因")
	}
	if got := m["ok"].Unavailable; got != "" {
		t.Errorf("Enabled=true 且依赖满足时 Unavailable 应为空，实际 %q", got)
	}
	// Requires 指向快照里被禁用的能力：即使本条启用也要写明未满足项。
	if got := m["dep-disabled"].Unavailable; !strings.Contains(got, "disabled-dep") ||
		strings.Contains(got, "enabled-dep") {
		t.Errorf("应只点名被禁用的依赖，实际 %q", got)
	}
}

func TestConcurrentListGetRefresh(t *testing.T) {
	specsA := baseSpecs()
	specsB := baseSpecs()
	for i := range specsB {
		specsB[i].Body = fmt.Sprintf("body-%d-v2", i)
	}
	f := &fakeSources{specs: specsA}
	r, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var wg sync.WaitGroup
	// 读路径：反复 List 两层 + Get 命中/未命中。
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 200; i++ {
				shallow, err := r.List(ctx, false)
				if err != nil {
					t.Errorf("List(false): %v", err)
					return
				}
				for _, it := range shallow {
					if it.Endpoint != "" || it.Body != "" || it.Requires != nil {
						t.Errorf("并发下 detail=false 泄漏详细字段：%+v", it)
						return
					}
				}
				if _, err := r.List(ctx, true); err != nil {
					t.Errorf("List(true): %v", err)
					return
				}
				if _, err := r.Get(ctx, "GET /v1/memory/search"); err != nil {
					t.Errorf("Get 命中: %v", err)
					return
				}
				if _, err := r.Get(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
					t.Errorf("Get 未命中应返回 ErrNotFound，实际 %v", err)
					return
				}
			}
		}()
	}
	// 写路径：反复换数据源 + Refresh。
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 60; i++ {
				if (i+id)%2 == 0 {
					f.set(specsA)
				} else {
					f.set(specsB)
				}
				if err := r.Refresh(ctx); err != nil {
					t.Errorf("Refresh: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// 收尾断言：无论刷新停在哪个版本，快照都完整且第一层披露成立。
	final, err := r.List(context.Background(), false)
	if err != nil || len(final) != len(specsA) {
		t.Fatalf("并发结束后 List 异常：len=%d err=%v", len(final), err)
	}
}
