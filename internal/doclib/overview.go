package doclib

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// renderOverview 实时拼「功能总览」。这里不做任何缓存：
// 能力开关会随运行时变化，缓存出来的总览等于骗人。
func renderOverview(ctx context.Context, src OverviewSource) string {
	var b strings.Builder
	b.WriteString("# 功能总览\n\n")
	b.WriteString("> " + overviewLead + "\n\n")

	writeRuntime(&b, src.Meta())

	feats, err := src.Features(ctx)
	if err != nil {
		// 文档页不该因为一个子系统挂了就打不开：降级成一段说明。
		b.WriteString("## 能力\n\n")
		b.WriteString("能力注册表不可用（" + tableCell(err.Error()) + "）\n")
		return b.String()
	}

	writeCapabilities(&b, feats)
	writeDisabled(&b, feats)
	return b.String()
}

// writeRuntime 输出 ## 运行时 表。Meta 为空时给一句占位，不留空表。
func writeRuntime(b *strings.Builder, meta map[string]string) {
	b.WriteString("## 运行时\n\n")
	if len(meta) == 0 {
		b.WriteString("_（无运行时信息）_\n\n")
		return
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("| 项 | 值 |\n| --- | --- |\n")
	for _, k := range keys {
		b.WriteString("| " + tableCell(k) + " | " + tableCell(meta[k]) + " |\n")
	}
	b.WriteString("\n")
}

type featureGroup struct {
	kind  string
	label string
}

var featureGroups = []featureGroup{
	{kind: "api", label: "API"},
	{kind: "cli", label: "CLI"},
	{kind: "skill", label: "技能"},
}

// writeCapabilities 按 api / cli / skill 分组输出。
func writeCapabilities(b *strings.Builder, feats []Feature) {
	buckets := map[string][]Feature{}
	var others []Feature
	for _, f := range feats {
		switch k := strings.ToLower(strings.TrimSpace(f.Kind)); k {
		case "api", "cli", "skill":
			buckets[k] = append(buckets[k], f)
		default:
			others = append(others, f)
		}
	}

	header := fmt.Sprintf("## 能力（共 %d 项：API %d / CLI %d / 技能 %d",
		len(feats), len(buckets["api"]), len(buckets["cli"]), len(buckets["skill"]))
	if len(others) > 0 {
		header += fmt.Sprintf(" / 其他 %d", len(others))
	}
	b.WriteString(header + "）\n\n")

	for _, g := range featureGroups {
		writeGroup(b, g, buckets[g.kind])
	}
	if len(others) > 0 {
		writeGroup(b, featureGroup{kind: "other", label: "其他"}, others)
	}
}

func writeGroup(b *strings.Builder, g featureGroup, items []Feature) {
	enabled := 0
	for _, f := range items {
		if f.Enabled {
			enabled++
		}
	}
	fmt.Fprintf(b, "### %s（%d 项，已启用 %d / 未启用 %d）\n\n",
		g.label, len(items), enabled, len(items)-enabled)
	if len(items) == 0 {
		b.WriteString("_（无）_\n\n")
		return
	}
	b.WriteString("| 名称 | 说明 | 状态 |\n| --- | --- | --- |\n")
	for _, f := range items {
		b.WriteString("| " + tableCell(f.Name) + " | " + tableCell(f.Description) +
			" | " + tableCell(featureStatus(f)) + " |\n")
	}
	b.WriteString("\n")
}

func writeDisabled(b *strings.Builder, feats []Feature) {
	b.WriteString("## 未启用的能力\n\n")
	var off []Feature
	for _, f := range feats {
		if !f.Enabled {
			off = append(off, f)
		}
	}
	if len(off) == 0 {
		b.WriteString("_（无，全部能力均已启用）_\n")
		return
	}
	for _, f := range off {
		reason := collapseSpace(f.Unavailable)
		if reason == "" {
			reason = "未说明原因"
		}
		fmt.Fprintf(b, "- **%s**（%s）：%s\n",
			collapseSpace(f.Name), kindLabel(f.Kind), reason)
	}
}

func featureStatus(f Feature) string {
	if f.Enabled {
		return "已启用"
	}
	if reason := collapseSpace(f.Unavailable); reason != "" {
		return "未启用：" + reason
	}
	return "未启用"
}

func kindLabel(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "api":
		return "API"
	case "cli":
		return "CLI"
	case "skill":
		return "技能"
	default:
		if k := strings.TrimSpace(kind); k != "" {
			return k
		}
		return "其他"
	}
}

// tableCell 把任意文本塞进一个 markdown 表格单元格：
// `|` 转义、换行压成空格、空值给 `-`。
func tableCell(s string) string {
	s = collapseSpace(s)
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}
