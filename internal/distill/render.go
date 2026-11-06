package distill

// 正文渲染 + 技能名生成。
//
// 所有内容都是事实拼装：步骤来自事件流，场景描述来自 run 的原始指令。
// 模板固定，所以同一个 run 反复沉淀会得到逐字节相同的文本——可 diff、可审计，
// 人改过的版本也能一眼看出改了什么。这是刻意不用 LLM 的直接收益。

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/store"
)

var (
	// 技能名只允许小写字母、数字和连字符，且不能以连字符开头/结尾。
	// 这条正则同时排除了 `/`、`\`、`.`、`..`——名字会拼进路径，必须是最严的那种。
	slugValidRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	slugKeepRe  = regexp.MustCompile(`[^a-z0-9-]+`)
	wordRe      = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*`)
)

// slug 里不带信息量的词。留着只会让技能名变成 "please-help-me-..."。
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true,
	"for": true, "of": true, "in": true, "on": true, "at": true, "by": true,
	"with": true, "please": true, "then": true, "this": true, "that": true,
	"it": true, "is": true, "me": true, "my": true, "can": true, "you": true,
	"help": true, "do": true, "run": true, "using": true, "use": true,
}

// SlugFrom 从 run 的指令里抠一个技能名。
//
// 中文指令抠不出 ASCII 词（不做拼音转换，转换结果没人能预期，还得引一个字典依赖），
// 这时退化成 task-<runID 前 6 位>：名字丑，但唯一、稳定、可读，人 accept 前能改。
func SlugFrom(input, runID string) string {
	var picked []string
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(input, -1) {
		w = strings.ToLower(w)
		if len(w) < 2 || stopWords[w] || seen[w] {
			continue
		}
		if len(w) > 20 {
			w = w[:20]
		}
		seen[w] = true
		picked = append(picked, w)
		if len(picked) == 3 {
			break
		}
	}
	slug := strings.Join(picked, "-")
	if !slugValidRe.MatchString(slug) || len(slug) < 3 || len(slug) > nameMaxLen {
		return fallbackSlug(runID)
	}
	return slug
}

// fallbackSlug 用 runID 前 6 位兜底。先截断再清洗：反过来会让清洗掉的字符
// 占掉配额，出现 "run-abc123" → "task-runabc" 这种和「前 6 位」对不上的结果。
func fallbackSlug(runID string) string {
	id := strings.ToLower(runID)
	if len(id) > 6 {
		id = id[:6]
	}
	id = strings.Trim(slugKeepRe.ReplaceAllString(id, ""), "-")
	if id == "" {
		id = "run"
	}
	return "task-" + id
}

// NormalizeName 校验并按需规范化用户给的技能名。
//
// 刻意**不**「自动修好」非法字符：名字是落盘路径的一部分，静默纠正会让人以为
// 落地位置和输入一致（例如 "../../etc/passwd" 修成 "etc-passwd" 后写进
// ~/.astra/skills/etc-passwd/——看着成功了，其实是另一个东西）。只接受
// 「空格/下划线换连字符」这一种无害的规范化，其余一律报错。
func NormalizeName(name string) (string, error) {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return "", fmt.Errorf("%w: 空名字", ErrBadName)
	}
	if strings.ContainsAny(raw, `/\`) || strings.Contains(raw, "..") {
		return "", fmt.Errorf("%w: %q 含路径分隔符或 ..", ErrBadName, name)
	}
	lower := strings.ToLower(raw)
	lower = strings.ReplaceAll(lower, "_", "-")
	lower = strings.ReplaceAll(lower, " ", "-")
	if len(lower) > nameMaxLen || !slugValidRe.MatchString(lower) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return lower, nil
}

// describe 是 frontmatter 里的 description：一句话，说明这个技能复用什么。
func describe(run store.Run) string {
	s := truncateRunes(singleLine(run.Input), 60)
	if s == "" {
		s = "一次已完成的 run"
	}
	return fmt.Sprintf("复用一次已完成任务的工具调用序列：%s", s)
}

// whenToUse 是「何时使用」那一段：从用户原话生成场景描述。
func whenToUse(run store.Run) string {
	s := truncateRunes(singleLine(run.Input), 80)
	if s == "" {
		return "当用户提出同类任务时，按下面的顺序执行。"
	}
	return fmt.Sprintf("当用户的要求与「%s」属于同一类任务时，按下面的顺序执行——"+
		"每一步的命令与路径都是当时真实用过的值，可以直接核对，不必重新摸索。", s)
}

// renderBody 生成完整的 SKILL.md（含 frontmatter）。
func renderBody(name, desc, when string, steps []Step, total, omitted int) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: %s\n", yamlString(desc))
	b.WriteString("version: 1\n")
	b.WriteString("requires: [astra-cli]\n")
	b.WriteString("---\n")

	b.WriteString("# 何时使用\n\n")
	b.WriteString(when)
	b.WriteString("\n\n# 步骤\n\n")
	if len(steps) == 0 {
		b.WriteString("（这次 run 没有留下可还原的工具调用。）\n\n")
	}
	for _, s := range steps {
		fmt.Fprintf(&b, "%d. `%s` — %s%s\n", s.Index, inlineCode(s.Tool), stepText(s), stepMark(s.Outcome))
	}

	// 截断必须在正文里说清楚：只列出前 N 步，人照着做会漏掉后半段。
	var tail []string
	if total > 0 {
		tail = append(tail, fmt.Sprintf("共 %d 次工具调用", total))
	}
	if omitted > 0 {
		tail = append(tail, fmt.Sprintf("另有 %d 步因超出步数上限未列出，需要补充", omitted))
	}
	if len(tail) > 0 {
		fmt.Fprintf(&b, "\n（%s。）\n", strings.Join(tail, "；"))
	}

	b.WriteString("\n# 注意\n\n")
	warns := warnings(steps)
	if len(warns) == 0 {
		b.WriteString("- 暂无\n")
	}
	for _, w := range warns {
		fmt.Fprintf(&b, "- %s\n", w)
	}
	return b.String()
}

func stepText(s Step) string {
	t := s.Summary
	if t == "" {
		t = s.Command
	}
	if t == "" {
		return "（无入参）"
	}
	return inlineCode(t)
}

func stepMark(outcome string) string {
	switch outcome {
	case OutcomeFailed:
		return "（失败）"
	case OutcomeDenied:
		return "（被拒绝）"
	case OutcomeOK:
		return ""
	}
	return "（未完成）"
}

// warnings 从失败/被拒绝的步骤里生成警示。
//
// 只写事实（哪一步、什么操作、什么结论），不猜原因：猜测会被人当成结论照抄，
// 而这里唯一可靠的信号是「这一步没成」。
func warnings(steps []Step) []string {
	var out []string
	for _, s := range steps {
		txt := truncateRunes(s.Summary, 80)
		if txt == "" {
			txt = s.Tool
		}
		switch s.Outcome {
		case OutcomeFailed:
			out = append(out, fmt.Sprintf("第 %d 步 `%s`（%s）当时**失败**了：%s —— "+
				"照做时先确认前置条件（文件是否存在、依赖是否装好），失败后先读报错再换做法。",
				s.Index, inlineCode(s.Tool), OutcomeFailed, inlineCode(txt)))
		case OutcomeDenied:
			out = append(out, fmt.Sprintf("第 %d 步 `%s`（%s）当时**被审批拒绝**：%s —— "+
				"这类操作需要人工放行，先确认审批策略或改成不需要授权的做法。",
				s.Index, inlineCode(s.Tool), OutcomeDenied, inlineCode(txt)))
		}
	}
	return out
}

// yamlString 输出一个 YAML 双引号标量。
//
// description 来自用户指令，可能带冒号、引号、井号——不加引号会被解析成
// map 或注释，整个 frontmatter 就错了（加载器会静默跳过这个技能）。
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// inlineCode 把一段文本包成 markdown 行内代码，顺带躲开反引号。
func inlineCode(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "`", "'")
	if s == "" {
		return "''"
	}
	return s
}

// singleLine 把多行命令压成一行：步骤清单里换行会把编号打断。
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	// 用 " ; " 而不是 " && "：后者在第一步失败时就短路，和原来的多行命令语义不同。
	return strings.Join(out, " ; ")
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
