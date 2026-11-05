package clikit

import (
	"bytes"
	"strings"
	"testing"
)

// SplitFlags 要同时支持 `cmd "指令" --wait` 与 `cmd --k 8 "关键词"`。
// 值型 flag 必须把后面的值一起吞掉，否则那个值会被当成位置参数。
func TestSplitFlags(t *testing.T) {
	takes := BoolFlags("k", "server")
	flags, pos := SplitFlags([]string{"--k", "8", "关键词", "--json"}, takes)
	if strings.Join(flags, " ") != "--k 8 --json" {
		t.Errorf("flags = %v", flags)
	}
	if strings.Join(pos, " ") != "关键词" {
		t.Errorf("pos = %v", pos)
	}

	// --k=8 自带值，不该吞掉下一个词
	flags, pos = SplitFlags([]string{"--k=8", "关键词"}, takes)
	if len(flags) != 1 || len(pos) != 1 || pos[0] != "关键词" {
		t.Errorf("flags=%v pos=%v", flags, pos)
	}

	// -- 之后全部当位置参数
	flags, pos = SplitFlags([]string{"--json", "--", "--not-a-flag"}, takes)
	if len(flags) != 1 || len(pos) != 1 || pos[0] != "--not-a-flag" {
		t.Errorf("flags=%v pos=%v", flags, pos)
	}
}

// 中文按两格算，否则列会整体右移。
func TestDisplayWidthAndTrunc(t *testing.T) {
	if got := DisplayWidth("租户"); got != 4 {
		t.Errorf("中文宽度应为 4，实际 %d", got)
	}
	if got := DisplayWidth("ab"); got != 2 {
		t.Errorf("ASCII 宽度应为 2，实际 %d", got)
	}
	// 截断后加省略号，且不切断宽字符
	got := Trunc("默认租户名称很长很长", 8)
	if DisplayWidth(got) > 8 {
		t.Errorf("截断后宽度超限: %q (%d)", got, DisplayWidth(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应带省略号: %q", got)
	}
}

// 表格在空数据时也要有 header + (空)，脚本才能区分「0 条」与「没跑」。
func TestTableRender(t *testing.T) {
	var buf bytes.Buffer
	tb := Table{Header: []string{"NAME", "SIZE"}, Right: map[int]bool{1: true}}
	tb.Rows = append(tb.Rows, []string{"租户", "12"})
	tb.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "租户") {
		t.Errorf("输出缺少内容: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Errorf("应为 header/分隔/1 行，实际 %d 行: %q", len(lines), out)
	}

	buf.Reset()
	empty := Table{Header: []string{"A"}}
	empty.Render(&buf)
	if !strings.Contains(buf.String(), "（空）") {
		t.Errorf("空表应显示（空）: %q", buf.String())
	}
}
