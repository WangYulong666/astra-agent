package doclib

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// stubOverview 是 OverviewSource 的测试替身。
type stubOverview struct {
	features []Feature
	featsErr error
	meta     map[string]string
}

func (s stubOverview) Features(context.Context) ([]Feature, error) { return s.features, s.featsErr }
func (s stubOverview) Meta() map[string]string                     { return s.meta }

func openLib(t *testing.T, cfg Config) Library {
	t.Helper()
	lib, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if lib == nil {
		t.Fatal("Open 返回 nil Library")
	}
	return lib
}

func writeDoc(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", rel, err)
	}
	return path
}

func docPaths(docs []Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Path
	}
	return out
}

// ── 分类 ────────────────────────────────────────────────────

func TestCategorize(t *testing.T) {
	cases := map[string]string{
		"design/self-evolving-loop.md":   "design",
		"design/sub/deep.md":             "design",
		"adr/0001-use-acp.md":            "adr",
		"adr/README.md":                  "adr",
		"history/架构设计方案-v1-已被否决.md":      "history",
		"使用说明.md":                        "guide",
		"cli.md":                         "guide",
		"README.md":                      "guide",
		"CONTRIBUTING.md":                "guide",
		"架构设计方案.md":                      "guide",
		"部署与流水线方案.md":                    "ops",
		"deploy.md":                      "ops",
		"完成度评估-2026-10-30.md":            "status",
		"可用级交付方案.md":                     "status",
		"平台化设计-二期.md":                    "status",
		"other/nested.md":                "guide",
		"iteration-review-2026-11-09.md": "guide",
	}
	for rel, want := range cases {
		if got := categorize(rel); got != want {
			t.Errorf("categorize(%q) = %q, 期望 %q", rel, got, want)
		}
	}
}

// ── 标题 / 摘要 ─────────────────────────────────────────────

func TestParseDoc(t *testing.T) {
	cases := []struct {
		name        string
		rel         string
		body        string
		wantTitle   string
		wantSummary string
	}{
		{
			name:        "无标题用文件名",
			rel:         "my-guide_doc.md",
			body:        "第一段正文。\n\n第二段。",
			wantTitle:   "my guide doc",
			wantSummary: "第一段正文。",
		},
		{
			name:        "代码块开头不当标题",
			rel:         "a.md",
			body:        "```\n# 不是标题\ncode\n```\n\n# 真标题\n\n正文段落。\n",
			wantTitle:   "真标题",
			wantSummary: "正文段落。",
		},
		{
			name:        "引用块跳过",
			rel:         "a.md",
			body:        "# 标题\n\n> 引用\n> 第二行\n\n真正摘要。\n",
			wantTitle:   "标题",
			wantSummary: "真正摘要。",
		},
		{
			name:        "去 markdown 标记",
			rel:         "a.md",
			body:        "# T\n\n这是一段 **重要** 说明，见 [`链接`](https://x/y) 与 `code`。\n",
			wantTitle:   "T",
			wantSummary: "这是一段 重要 说明，见 链接 与 code。",
		},
		{
			name:        "列表不算段落",
			rel:         "a.md",
			body:        "# T\n\n- 一\n- 二\n\n正文。\n",
			wantTitle:   "T",
			wantSummary: "正文。",
		},
		{
			name:        "表格跳过",
			rel:         "a.md",
			body:        "# T\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n段落。\n",
			wantTitle:   "T",
			wantSummary: "段落。",
		},
		{
			name:        "frontmatter 跳过",
			rel:         "a.md",
			body:        "---\ntitle: x\n---\n\n# 真标题\n\n摘要。\n",
			wantTitle:   "真标题",
			wantSummary: "摘要。",
		},
		{
			name:        "中文折行不塞空格",
			rel:         "a.md",
			body:        "# T\n\n第一行文字\n第二行文字\n",
			wantTitle:   "T",
			wantSummary: "第一行文字第二行文字",
		},
		{
			name:        "ATX 关闭式标题",
			rel:         "a.md",
			body:        "# 标题 #\n\n正文。\n",
			wantTitle:   "标题",
			wantSummary: "正文。",
		},
		{
			name:        "空文件",
			rel:         "empty_doc.md",
			body:        "",
			wantTitle:   "empty doc",
			wantSummary: "",
		},
		{
			name:        "只有标题",
			rel:         "a.md",
			body:        "# T\n\n## 小节\n",
			wantTitle:   "T",
			wantSummary: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseDoc(tc.rel, tc.body)
			if p.title != tc.wantTitle {
				t.Errorf("Title = %q, 期望 %q", p.title, tc.wantTitle)
			}
			if p.summary != tc.wantSummary {
				t.Errorf("Summary = %q, 期望 %q", p.summary, tc.wantSummary)
			}
			if tc.rel == "my-guide_doc.md" && p.category != "guide" {
				t.Errorf("Category = %q, 期望 guide", p.category)
			}
		})
	}
}

func TestSummaryTruncatesByRune(t *testing.T) {
	body := "# T\n\n" + strings.Repeat("中", 200)
	p := parseDoc("a.md", body)
	runes := []rune(p.summary)
	if len(runes) != summaryLimit+1 {
		t.Fatalf("摘要 rune 数 = %d, 期望 %d（120 + 省略号）", len(runes), summaryLimit+1)
	}
	if runes[len(runes)-1] != '…' {
		t.Errorf("截断后应以 … 结尾，实际 %q", string(runes[len(runes)-1]))
	}
	if !utf8.ValidString(p.summary) {
		t.Error("摘要不是合法 UTF-8：截断切坏了 rune")
	}
	if strings.ContainsRune(p.summary, utf8.RuneError) {
		t.Error("摘要里出现替换字符，说明按字节截断了")
	}
}

// ── Index ───────────────────────────────────────────────────

func TestIndexCategoriesAndOrder(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "使用说明.md", "# 使用说明\n\n上手。\n")
	writeDoc(t, root, "cli.md", "# CLI 使用文档\n\n命令。\n")
	writeDoc(t, root, "部署与流水线方案.md", "# 部署\n\n流水线。\n")
	writeDoc(t, root, "完成度评估.md", "# 完成度\n\n评估。\n")
	writeDoc(t, root, "design/x.md", "# Zulu\n\n设计。\n")
	writeDoc(t, root, "design/a.md", "# Alpha\n\n设计。\n")
	writeDoc(t, root, "adr/0001.md", "# 0001. 决策\n\n背景。\n")
	writeDoc(t, root, "history/old.md", "# 旧方案\n\n历史。\n")
	// 应被跳过的目录
	writeDoc(t, root, ".hidden/x.md", "# 隐藏\n\n不该出现。\n")
	writeDoc(t, root, "node_modules/pkg/readme.md", "# NM\n\n不该出现。\n")
	writeDoc(t, root, "third_party/lib/doc.md", "# TP\n\n不该出现。\n")

	lib := openLib(t, Config{Root: root})
	docs, err := lib.Index(context.Background())
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	want := []string{
		"cli.md", "使用说明.md",
		"design/a.md", "design/x.md",
		"adr/0001.md",
		"部署与流水线方案.md",
		"完成度评估.md",
		"history/old.md",
	}
	got := docPaths(docs)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Index 路径顺序 = %v\n期望 %v", got, want)
	}
	for _, d := range docs {
		if d.Body != "" {
			t.Errorf("%s: Index 不应带 Body", d.Path)
		}
		if d.Summary == "" {
			t.Errorf("%s: Summary 为空", d.Path)
		}
		if d.UpdatedAt.IsZero() {
			t.Errorf("%s: UpdatedAt 为零值", d.Path)
		}
	}
	if docs[2].Title != "Alpha" || docs[3].Title != "Zulu" {
		t.Errorf("同类应按标题排序，实际 %q / %q", docs[2].Title, docs[3].Title)
	}
}

func TestIndexSkipsBrokenFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视文件权限，无法构造不可读文件")
	}
	root := t.TempDir()
	writeDoc(t, root, "good.md", "# 好文件\n\n正文。\n")
	bad := writeDoc(t, root, "bad.md", "# 坏文件\n\n正文。\n")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	lib := openLib(t, Config{Root: root})
	docs, err := lib.Index(context.Background())
	if err != nil {
		t.Fatalf("Index 遇到坏文件不该报错: %v", err)
	}
	got := docPaths(docs)
	if strings.Join(got, ",") != "good.md" {
		t.Fatalf("Index = %v, 期望只有 good.md", got)
	}
	if _, err := lib.Get(context.Background(), "bad.md"); err == nil {
		t.Fatal("Get 坏文件应返回错误")
	}
}

// ── 路径安全 ────────────────────────────────────────────────

func TestGetRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	writeDoc(t, root, "good.md", "# 好\n\n正文。\n")
	secret := writeDoc(t, outside, "secret.md", "# 秘密\n\n不该被读到。\n")

	if err := os.Symlink(secret, filepath.Join(root, "link.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatalf("Symlink dir: %v", err)
	}

	lib := openLib(t, Config{Root: root})
	ctx := context.Background()

	cases := []string{
		"../outside/secret.md",
		"sub/../../outside/secret.md",
		secret,
		filepath.Join(root, "good.md"),
		"/etc/passwd",
		"link.md",
		"dirlink/secret.md",
		"",
		".",
		"..",
		"good.txt",
		"good.md/../../outside/secret.md",
	}
	for _, p := range cases {
		if _, err := lib.Get(ctx, p); err != ErrNotFound {
			t.Errorf("Get(%q) 错误 = %v, 期望 ErrNotFound", p, err)
		}
	}

	doc, err := lib.Get(ctx, "good.md")
	if err != nil {
		t.Fatalf("Get(good.md): %v", err)
	}
	if doc.Title != "好" || !strings.Contains(doc.Body, "正文") {
		t.Errorf("Get(good.md) = %+v", doc)
	}

	// 索引也不能把根外软链泄露出去。
	docs, err := lib.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	for _, d := range docs {
		if d.Path == "link.md" || d.Path == "dirlink/secret.md" {
			t.Errorf("Index 泄露了根外软链：%s", d.Path)
		}
	}
}

func TestSymlinkInsideRootWorks(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "real.md", "# 真身\n\n内容。\n")
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	lib := openLib(t, Config{Root: root})
	doc, err := lib.Get(context.Background(), "alias.md")
	if err != nil {
		t.Fatalf("根内软链应可读: %v", err)
	}
	if doc.Title != "真身" {
		t.Errorf("Title = %q", doc.Title)
	}
}

// ── 边界：Root 为空 / 不存在 ─────────────────────────────────

func TestRootMissingOrEmpty(t *testing.T) {
	ctx := context.Background()
	src := stubOverview{meta: map[string]string{"provider": "deepseek"}}

	for _, root := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		lib := openLib(t, Config{Root: root, Overview: src})
		docs, err := lib.Index(ctx)
		if err != nil {
			t.Fatalf("Root=%q Index: %v", root, err)
		}
		if len(docs) != 1 || !docs[0].Generated || docs[0].Category != "overview" {
			t.Fatalf("Root=%q Index = %+v, 期望只有生成的总览", root, docs)
		}
		if _, err := lib.Get(ctx, "a.md"); err != ErrNotFound {
			t.Errorf("Root=%q Get 应 ErrNotFound，实际 %v", root, err)
		}
		if items, err := lib.Search(ctx, "任何", 10); err != nil || len(items) != 0 {
			t.Errorf("Root=%q Search = %v, %v", root, items, err)
		}
	}

	// 没有 Overview 时 Index 返回空列表而不是错误。
	lib := openLib(t, Config{Root: filepath.Join(t.TempDir(), "nope")})
	docs, err := lib.Index(ctx)
	if err != nil || len(docs) != 0 {
		t.Fatalf("无 Root 无 Overview: %v, %v", docs, err)
	}
}

// ── 缓存 ────────────────────────────────────────────────────

func TestCacheInvalidatesOnChange(t *testing.T) {
	root := t.TempDir()
	path := writeDoc(t, root, "a.md", "# 一\n\n旧正文。\n")
	lib := openLib(t, Config{Root: root})
	ctx := context.Background()

	doc, err := lib.Get(ctx, "a.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if doc.Title != "一" || !strings.Contains(doc.Body, "旧正文") {
		t.Fatalf("首次 Get = %+v", doc)
	}

	writeDoc(t, root, "a.md", "# 二\n\n新正文，更长一些。\n")
	future := doc.UpdatedAt.Add(2 * 1e9)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	doc, err = lib.Get(ctx, "a.md")
	if err != nil {
		t.Fatalf("第二次 Get: %v", err)
	}
	if doc.Title != "二" || !strings.Contains(doc.Body, "新正文") {
		t.Fatalf("缓存未失效，Get = %+v", doc)
	}

	docs, err := lib.Index(ctx)
	if err != nil || len(docs) != 1 || docs[0].Title != "二" {
		t.Fatalf("Index 未反映变化: %+v, %v", docs, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	docs, err = lib.Index(ctx)
	if err != nil || len(docs) != 0 {
		t.Fatalf("删除后 Index = %+v, %v", docs, err)
	}
	if _, err := lib.Get(ctx, "a.md"); err != ErrNotFound {
		t.Errorf("删除后 Get 应 ErrNotFound，实际 %v", err)
	}
}

func TestContextCanceled(t *testing.T) {
	lib := openLib(t, Config{Root: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lib.Index(ctx); err == nil {
		t.Error("Index 应返回 context 错误")
	}
	if _, err := lib.Get(ctx, "a.md"); err == nil {
		t.Error("Get 应返回 context 错误")
	}
	if _, err := lib.Search(ctx, "x", 1); err == nil {
		t.Error("Search 应返回 context 错误")
	}
}
