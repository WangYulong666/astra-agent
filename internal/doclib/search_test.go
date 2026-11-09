package doclib

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSearchOrderAndShape(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "a.md", "# Alpha\n\nalpha alpha beta\n")
	writeDoc(t, root, "b.md", "# Beta\n\nalpha\n")
	writeDoc(t, root, "c.md", "# Gamma\n\nalpha beta alpha alpha alpha\n")

	lib := openLib(t, Config{Root: root})
	docs, err := lib.Search(context.Background(), "alpha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := docPaths(docs)
	want := []string{"c.md", "a.md", "b.md"} // 命中次数 4 / 3 / 1
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Search 顺序 = %v, 期望 %v", got, want)
	}
	for _, d := range docs {
		if d.Body != "" {
			t.Errorf("%s: 搜索结果不应带 Body", d.Path)
		}
		if !strings.Contains(strings.ToLower(d.Summary), "alpha") {
			t.Errorf("%s: 摘要里应能看到命中词，实际 %q", d.Path, d.Summary)
		}
	}

	// 大小写不敏感。
	up, err := lib.Search(context.Background(), "ALPHA", 10)
	if err != nil {
		t.Fatalf("Search 大写: %v", err)
	}
	if len(up) != len(docs) {
		t.Errorf("大写查询结果数 = %d, 期望 %d", len(up), len(docs))
	}
}

func TestSearchSnippetAroundHit(t *testing.T) {
	root := t.TempDir()
	body := "# S\n\n" + strings.Repeat("前置内容", 30) + " 关键命中 " + strings.Repeat("后置内容", 30) + "\n"
	writeDoc(t, root, "s.md", body)

	lib := openLib(t, Config{Root: root})
	docs, err := lib.Search(context.Background(), "关键命中", 5)
	if err != nil || len(docs) != 1 {
		t.Fatalf("Search = %+v, %v", docs, err)
	}
	s := docs[0].Summary
	if !strings.Contains(s, "关键命中") {
		t.Fatalf("摘要未定位到命中: %q", s)
	}
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") {
		t.Errorf("长正文的摘要应两头带省略号: %q", s)
	}
	runes := []rune(s)
	max := searchSnippetPad*2 + len([]rune("关键命中")) + 2
	if len(runes) > max {
		t.Errorf("摘要长度 %d 超过上限 %d", len(runes), max)
	}
}

func TestSearchLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		writeDoc(t, root, fmt.Sprintf("d%02d.md", i), fmt.Sprintf("# Doc %02d\n\nneedle\n", i))
	}
	lib := openLib(t, Config{Root: root})
	ctx := context.Background()

	if docs, _ := lib.Search(ctx, "needle", 5); len(docs) != 5 {
		t.Errorf("limit=5 返回 %d 条", len(docs))
	}
	if docs, _ := lib.Search(ctx, "needle", 0); len(docs) != searchDefaultLimit {
		t.Errorf("limit=0 应默认 %d，实际 %d", searchDefaultLimit, len(docs))
	}
	if docs, _ := lib.Search(ctx, "needle", -3); len(docs) != searchDefaultLimit {
		t.Errorf("limit<0 应默认 %d，实际 %d", searchDefaultLimit, len(docs))
	}
	if docs, _ := lib.Search(ctx, "needle", 1000); len(docs) != 25 {
		t.Errorf("limit 超上限时不该截断到更少，实际 %d", len(docs))
	}
}

func TestSearchLimitCapsAt100(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 105; i++ {
		writeDoc(t, root, fmt.Sprintf("d%03d.md", i), fmt.Sprintf("# Doc %03d\n\nneedle\n", i))
	}
	lib := openLib(t, Config{Root: root})
	docs, err := lib.Search(context.Background(), "needle", 1000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(docs) != searchMaxLimit {
		t.Errorf("limit 上限 = %d，实际返回 %d", searchMaxLimit, len(docs))
	}
}

func TestSearchEmptyAndNoMatch(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "a.md", "# A\n\n正文。\n")
	lib := openLib(t, Config{Root: root})
	ctx := context.Background()

	for _, q := range []string{"", "   ", "\n\t"} {
		docs, err := lib.Search(ctx, q, 10)
		if err != nil {
			t.Fatalf("空查询不该报错: %v", err)
		}
		if len(docs) != 0 {
			t.Errorf("空查询 %q 返回 %d 条", q, len(docs))
		}
	}

	docs, err := lib.Search(ctx, "不存在的词", 10)
	if err != nil || len(docs) != 0 {
		t.Errorf("无命中 = %+v, %v", docs, err)
	}
}

func TestSearchTieBreaksByTitle(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "z.md", "# Zulu\n\nword\n")
	writeDoc(t, root, "a.md", "# Alpha\n\nword\n")
	writeDoc(t, root, "m.md", "# Mike\n\nword\n")

	lib := openLib(t, Config{Root: root})
	docs, err := lib.Search(context.Background(), "word", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := docPaths(docs)
	if strings.Join(got, ",") != "a.md,m.md,z.md" {
		t.Errorf("同命中次数应按标题排序，实际 %v", got)
	}
}

func TestSearchStripsMarkdownFromSnippet(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "a.md", "# A\n\n这里 **加粗命中** 与 `代码`。\n")
	lib := openLib(t, Config{Root: root})
	docs, err := lib.Search(context.Background(), "命中", 5)
	if err != nil || len(docs) != 1 {
		t.Fatalf("Search = %+v, %v", docs, err)
	}
	if strings.Contains(docs[0].Summary, "**") {
		t.Errorf("摘要里残留 markdown 标记: %q", docs[0].Summary)
	}
}
