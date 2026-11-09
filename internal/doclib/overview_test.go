package doclib

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var testFeatures = []Feature{
	{Name: "ask", Kind: "api", Description: "问答", Enabled: true, Source: "core"},
	{Name: "chat", Kind: "api", Description: "管道 | 换行\n第二行", Enabled: false, Unavailable: "缺少 key", Source: "core"},
	{Name: "scan", Kind: "cli", Description: "", Enabled: true, Source: "cli"},
	{Name: "memory", Kind: "skill", Description: "记忆", Enabled: false, Unavailable: "provider 未配置"},
}

func TestOverviewGenerated(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "cli.md", "# CLI\n\n命令。\n")

	src := stubOverview{
		features: testFeatures,
		meta:     map[string]string{"provider": "deepseek", "agent": "astra", "count": "3", "empty": ""},
	}
	lib := openLib(t, Config{Root: root, Overview: src})
	ctx := context.Background()

	docs, err := lib.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(docs) != 2 || !docs[0].Generated || docs[0].Path != "overview" || docs[0].Title != "功能总览" {
		t.Fatalf("生成的总览应固定在最前，实际 %+v", docs)
	}
	if docs[0].UpdatedAt.IsZero() {
		t.Error("总览 UpdatedAt 不应为零值")
	}

	doc, err := lib.Get(ctx, "overview")
	if err != nil {
		t.Fatalf("Get(overview): %v", err)
	}
	if !doc.Generated || doc.Category != "overview" || doc.Path != "overview" {
		t.Fatalf("Get(overview) 元数据 = %+v", doc)
	}
	body := doc.Body

	wants := []string{
		"# 功能总览",
		"> " + overviewLead,
		"## 运行时",
		"| agent | astra |",
		"| count | 3 |",
		"| empty | - |",
		"| provider | deepseek |",
		"## 能力（共 4 项：API 2 / CLI 1 / 技能 1）",
		"### API（2 项，已启用 1 / 未启用 1）",
		"| ask | 问答 | 已启用 |",
		`| chat | 管道 \| 换行 第二行 | 未启用：缺少 key |`,
		"### CLI（1 项，已启用 1 / 未启用 0）",
		"| scan | - | 已启用 |",
		"### 技能（1 项，已启用 0 / 未启用 1）",
		"## 未启用的能力",
		"- **chat**（API）：缺少 key",
		"- **memory**（技能）：provider 未配置",
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("总览缺少 %q\n---\n%s", w, body)
		}
	}

	// 运行时按 key 排序。
	order := []string{"| agent |", "| count |", "| empty |", "| provider |"}
	last := -1
	for _, k := range order {
		i := strings.Index(body, k)
		if i < 0 {
			t.Fatalf("找不到 %q", k)
		}
		if i < last {
			t.Errorf("运行时键未按字典序排列：%q 位置 %d < %d", k, i, last)
		}
		last = i
	}

	// 未启用的能力只列未启用的。
	disabled := body[strings.Index(body, "## 未启用的能力"):]
	if strings.Contains(disabled, "ask") || strings.Contains(disabled, "scan") {
		t.Errorf("未启用区块混进了已启用能力:\n%s", disabled)
	}
	if strings.Contains(body, "| chat | 管道 | 换行") {
		t.Error("表格里的 | 没有转义，会把列打乱")
	}
}

func TestOverviewMetaEmpty(t *testing.T) {
	lib := openLib(t, Config{Overview: stubOverview{}})
	doc, err := lib.Get(context.Background(), "overview")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(doc.Body, "## 运行时") || !strings.Contains(doc.Body, "（无运行时信息）") {
		t.Errorf("Meta 为空时缺少占位:\n%s", doc.Body)
	}
	if !strings.Contains(doc.Body, "## 能力（共 0 项：API 0 / CLI 0 / 技能 0）") {
		t.Errorf("空能力的计数头不对:\n%s", doc.Body)
	}
	if !strings.Contains(doc.Body, "## 未启用的能力") {
		t.Errorf("缺少未启用区块:\n%s", doc.Body)
	}
}

func TestOverviewDegradesWhenRegistryFails(t *testing.T) {
	src := stubOverview{
		features: testFeatures,
		featsErr: errors.New("与能力注册表断开"),
		meta:     map[string]string{"provider": "deepseek"},
	}
	lib := openLib(t, Config{Overview: src})

	doc, err := lib.Get(context.Background(), "overview")
	if err != nil {
		t.Fatalf("能力注册表挂了不该让文档页失败: %v", err)
	}
	if !strings.Contains(doc.Body, "# 功能总览") {
		t.Errorf("降级后仍应有标题:\n%s", doc.Body)
	}
	if !strings.Contains(doc.Body, "## 运行时") || !strings.Contains(doc.Body, "| provider | deepseek |") {
		t.Errorf("降级后运行时信息应保留:\n%s", doc.Body)
	}
	if !strings.Contains(doc.Body, "能力注册表不可用（与能力注册表断开）") {
		t.Errorf("缺少降级说明:\n%s", doc.Body)
	}
	if strings.Contains(doc.Body, "### API") {
		t.Errorf("降级后不应再输出分组表:\n%s", doc.Body)
	}
}

func TestOverviewDisabledWithoutSource(t *testing.T) {
	root := t.TempDir()
	writeDoc(t, root, "a.md", "# A\n\n正文。\n")
	lib := openLib(t, Config{Root: root})

	docs, err := lib.Index(context.Background())
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(docs) != 1 || docs[0].Generated {
		t.Fatalf("Overview 为 nil 时不该有生成页: %+v", docs)
	}
	if _, err := lib.Get(context.Background(), "overview"); err != ErrNotFound {
		t.Errorf("Overview 为 nil 时 Get(overview) 应 ErrNotFound，实际 %v", err)
	}
}

func TestOverviewCustomPath(t *testing.T) {
	src := stubOverview{meta: map[string]string{"k": "v"}}
	lib := openLib(t, Config{Overview: src, OverviewPath: "generated/功能总览"})
	ctx := context.Background()

	docs, err := lib.Index(ctx)
	if err != nil || len(docs) != 1 {
		t.Fatalf("Index = %+v, %v", docs, err)
	}
	if docs[0].Path != "generated/功能总览" {
		t.Fatalf("自定义路径 = %q", docs[0].Path)
	}
	if _, err := lib.Get(ctx, "/generated/功能总览"); err != nil {
		t.Errorf("自定义路径应可访问: %v", err)
	}
}

func TestOverviewReflectsLiveChanges(t *testing.T) {
	// 总览不能缓存：同一个 Library，注册表开关变了，正文要跟着变。
	enabled := true
	src := &mutableOverview{on: &enabled}
	lib := openLib(t, Config{Overview: src})
	ctx := context.Background()

	first, err := lib.Get(ctx, "overview")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(first.Body, "已启用 1 / 未启用 0") {
		t.Fatalf("首次总览不对:\n%s", first.Body)
	}

	enabled = false
	second, err := lib.Get(ctx, "overview")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(second.Body, "已启用 0 / 未启用 1") {
		t.Errorf("总览被缓存了，没有反映最新开关:\n%s", second.Body)
	}
}

type mutableOverview struct{ on *bool }

func (m *mutableOverview) Features(context.Context) ([]Feature, error) {
	return []Feature{{Name: "x", Kind: "api", Description: "d", Enabled: *m.on}}, nil
}
func (m *mutableOverview) Meta() map[string]string { return nil }

func TestOverviewUpdatedAtUsesClock(t *testing.T) {
	lib := openLib(t, Config{Overview: stubOverview{}}).(*library)
	fixed := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	lib.now = func() time.Time { return fixed }
	doc, err := lib.Get(context.Background(), "overview")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !doc.UpdatedAt.Equal(fixed) {
		t.Errorf("UpdatedAt = %v, 期望 %v", doc.UpdatedAt, fixed)
	}
}
