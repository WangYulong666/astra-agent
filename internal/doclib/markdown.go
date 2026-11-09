package doclib

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	// summaryLimit 是摘要的 rune 上限。列表页只给一瞥，正文走 Get。
	summaryLimit = 120
	// overviewLead 是生成页顶部那句「别信手写清单」的说明。
	overviewLead = "本页由能力注册表实时生成，反映当前这份二进制的真实状态，不是手写清单。"
)

// parsed 是一篇 markdown 的解析结果。Body 保留原始文件内容，落库给全文用。
type parsed struct {
	title    string
	summary  string
	category string
	body     string
}

func parseDoc(rel, body string) parsed {
	body = strings.TrimPrefix(body, "\ufeff")
	return parsed{
		title:    extractTitle(rel, body),
		summary:  extractSummary(body),
		category: categorize(rel),
		body:     body,
	}
}

// ── 标题 ────────────────────────────────────────────────────

func extractTitle(rel, body string) string {
	inFence, inFront := false, false
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFront = true
			continue
		}
		if inFront {
			if trimmed == "---" || trimmed == "..." {
				inFront = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || isIndentedCode(line) {
			continue
		}
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		// 允许 ATX 关闭式写法 "# 标题 #"。
		title = strings.TrimSpace(strings.TrimRight(title, "#"))
		if title == "" {
			continue
		}
		return stripMarkdown(title)
	}
	return fileTitle(rel)
}

// fileTitle 用文件名兜底：去 .md，-/_ 换空格。
func fileTitle(rel string) string {
	base := filepath.Base(filepath.ToSlash(rel))
	if strings.HasSuffix(strings.ToLower(base), ".md") {
		base = base[:len(base)-3]
	}
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	return strings.Join(strings.Fields(base), " ")
}

// ── 摘要 ────────────────────────────────────────────────────

// extractSummary 取第一个「像段落」的块：非空、非标题、非代码、非引用、
// 非表格、非列表、非分割线。多行合并，去掉 markdown 标记后按 rune 截断。
func extractSummary(body string) string {
	inFence, inFront := false, false
	var para []string
	paraRunes := 0

	flush := func() string {
		if len(para) == 0 {
			return ""
		}
		text := collapseSpace(stripMarkdown(joinParagraph(para)))
		para = nil
		paraRunes = 0
		return text
	}

	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFront = true
			continue
		}
		if inFront {
			if trimmed == "---" || trimmed == "..." {
				inFront = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if s := flush(); s != "" {
				return truncateRunes(s, summaryLimit)
			}
			inFence = !inFence
			continue
		}
		if inFence || isIndentedCode(line) {
			continue
		}
		if trimmed == "" {
			if s := flush(); s != "" {
				return truncateRunes(s, summaryLimit)
			}
			continue
		}
		if isBlockLine(trimmed) {
			// 结构行截断当前段落；若已有段落就收工，否则继续往后找。
			if s := flush(); s != "" {
				return truncateRunes(s, summaryLimit)
			}
			continue
		}
		para = append(para, trimmed)
		paraRunes += len([]rune(trimmed))
		if paraRunes >= 400 {
			if s := flush(); s != "" {
				return truncateRunes(s, summaryLimit)
			}
		}
	}
	if s := flush(); s != "" {
		return truncateRunes(s, summaryLimit)
	}
	return ""
}

// isIndentedCode 判断四空格/tab 缩进的代码块。必须在 TrimSpace 之前调用。
func isIndentedCode(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

// isBlockLine 判断不属于「段落」的 markdown 块起始行。
func isBlockLine(line string) bool {
	if isOrderedList(line) {
		return true
	}
	switch {
	case strings.HasPrefix(line, "#"),
		strings.HasPrefix(line, ">"),
		strings.HasPrefix(line, "|"),
		strings.HasPrefix(line, "<"),
		strings.HasPrefix(line, "---"),
		strings.HasPrefix(line, "***"),
		strings.HasPrefix(line, "___"),
		strings.HasPrefix(line, "==="),
		strings.HasPrefix(line, "- "),
		strings.HasPrefix(line, "* "),
		strings.HasPrefix(line, "+ "),
		strings.HasPrefix(line, "• "):
		return true
	}
	return false
}

// isOrderedList 匹配 "1. " / "12) "。
func isOrderedList(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) {
		return false
	}
	if line[i] != '.' && line[i] != ')' {
		return false
	}
	return i+1 < len(line) && line[i+1] == ' '
}

// joinParagraph 合并段落的折行。中文之间不塞空格，避免「这 是 一句 话」。
func joinParagraph(lines []string) string {
	var b strings.Builder
	var last rune
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if b.Len() > 0 {
			next := []rune(l)[0]
			if !isCJK(last) && !isCJK(next) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(l)
		r := []rune(l)
		last = r[len(r)-1]
	}
	return b.String()
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// ── markdown 标记 ───────────────────────────────────────────

var linkRe = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)

// stripMarkdown 只做展示层去噪：链接留文字、去加粗/行内代码标记。
func stripMarkdown(s string) string {
	s = linkRe.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	return s
}

// truncateRunes 按 rune 截断，保证不会把中文/emoji 切坏。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// snippetAround 在 text 里定位 q（大小写不敏感），取命中前后各 radius 个 rune。
// 搜索结果靠它解释「为什么命中」。
func snippetAround(text, q string, radius int) string {
	if q == "" {
		return ""
	}
	lower := strings.ToLower(text)
	idx := strings.Index(lower, strings.ToLower(q))
	if idx < 0 {
		return ""
	}
	runes := []rune(text)
	start := len([]rune(text[:idx]))
	end := start + len([]rune(q))
	lo, hi := start-radius, end+radius
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	var b strings.Builder
	if lo > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(runes[lo:hi]))
	if hi < len(runes) {
		b.WriteString("…")
	}
	return b.String()
}

// ── 分类 ────────────────────────────────────────────────────

var categoryOrder = map[string]int{
	"overview": 0,
	"guide":    1,
	"design":   2,
	"adr":      3,
	"ops":      4,
	"status":   5,
	"history":  6,
}

// categorize 只看相对路径：目录优先，根级文件按文件名关键词。
func categorize(rel string) string {
	slash := filepath.ToSlash(rel)
	if i := strings.IndexByte(slash, '/'); i >= 0 {
		switch strings.ToLower(slash[:i]) {
		case "design":
			return "design"
		case "adr":
			return "adr"
		case "history":
			return "history"
		}
		return "guide"
	}
	base := strings.ToLower(fileTitle(slash))
	switch {
	case strings.Contains(base, "使用说明"),
		strings.Contains(base, "cli"),
		strings.Contains(base, "readme"),
		strings.Contains(base, "contributing"):
		return "guide"
	case strings.Contains(base, "部署"), strings.Contains(base, "deploy"):
		return "ops"
	case strings.Contains(base, "完成度"),
		strings.Contains(base, "可用级"),
		strings.Contains(base, "平台化设计"):
		return "status"
	}
	return "guide"
}
