package main

import (
	"io"
	"os"
	"strings"
	"time"
)

// in 返回 stdin。抽成方法是为了让测试能替换输入（确认提示）。
func (a *app) in() io.Reader { return os.Stdin }

// formatTime 统一时间显示：本地时区、到分钟。零值显示 "-"，
// 否则会看到一个 0001-01-01，比空白更让人困惑。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// shortID 长 id 在表格里截一半，前缀足够区分。
func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// oneLine 把多行文本压成一行（表格用）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	return strings.TrimSpace(s)
}

// summarizeInput 把工具入参压成一行摘要。rawInput 是任意 JSON，
// 这里只做「紧凑化 + 截断」，不试图理解 schema。
func summarizeInput(raw []byte, max int) string {
	if len(raw) == 0 {
		return ""
	}
	s := compactJSON(raw)
	if s == "" || s == "null" || s == "{}" {
		return ""
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// compactJSON 去掉 JSON 里的空白，让一行能塞下更多信息。
func compactJSON(raw []byte) string {
	var b strings.Builder
	inStr := false
	esc := false
	for _, r := range string(raw) {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case r == '\\' && inStr:
			b.WriteRune(r)
			esc = true
		case r == '"':
			inStr = !inStr
			b.WriteRune(r)
		case (r == ' ' || r == '\n' || r == '\t' || r == '\r') && !inStr:
			// 丢弃
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
