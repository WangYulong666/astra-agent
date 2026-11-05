// Package clikit 是两个 CLI 共用的「不是 HTTP 的那一半」：配置文件、表格、
// JSON 输出、参数切分、确认提示。
//
// 为什么放在 internal/client 下：它与 internal/client 是同一份交付物（客户端侧），
// 而设计文档把 internal/client/** 划给了 CLI 这一路，放在这里不会踩到别人的文件。
// 为什么不各 CLI 写一份：两边的表格宽度算法、0600 权限、--json 行为一旦漂移，
// 「同一份数据两个工具显示不一样」是最容易招骂的 bug。
package clikit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// ── 配置 ─────────────────────────────────────────────────────

// Config 是 CLI 的本地配置。两个 CLI 各存一份文件，避免用低权限 token
// 去执行管理命令（或反过来，把 admin token 留在用户侧）。
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	Email  string `json:"email,omitempty"`
	// Tenant 是登录用的租户 slug（服务端 login 需要它），默认 default。
	Tenant string `json:"tenant,omitempty"`
	// Role 只用于提示「当前这份配置是什么身份」，不参与鉴权。
	Role string `json:"role,omitempty"`
}

// Dir 返回 ~/.astra（不存在则创建，权限 0700）。
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("找不到用户主目录: %w", err)
	}
	dir := filepath.Join(home, ".astra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	return dir, nil
}

// Path 返回 ~/.astra/<name> 的绝对路径。
func Path(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// ExpandHome 把前缀 ~ 展开成主目录（--dest '~/x' 这种写法要能用）。
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// LoadConfig 读配置。文件不存在不是错误——第一次用 CLI 时必然没有配置，
// 这时应该给出「请先 login」的提示，而不是一个 IO 错误。
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("读配置 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("配置 %s 不是合法 JSON（可删掉重新 login）: %w", path, err)
	}
	return c, nil
}

// SaveConfig 写配置。权限必须是 0600：文件里有明文 token，
// 默认 0644 会让同机器上的其他用户直接读到。
func SaveConfig(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// 先写临时文件再 rename：进程被 Ctrl-C 打断时不会留下半个 JSON，
	// 否则下次启动会因为「配置不是合法 JSON」直接不可用。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写 %s 失败: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

// ── 输出 ─────────────────────────────────────────────────────

// PrintJSON 输出缩进 JSON（给脚本/agent 用的原始结构）。
func PrintJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Table 是最小可用的等宽表格。不引第三方库：CLI 的输出格式是契约，
// 用库反而要跟着库的版本改。
type Table struct {
	Header []string
	Rows   [][]string
	// 右对齐的列下标（数字列）。中文按两格宽计算，否则列会歪。
	Right map[int]bool
}

// Render 打印表格。空数据时打印 header + 一行「（空）」，让脚本能判断
// 「查到 0 条」和「命令没跑」的区别。
func (t *Table) Render(w io.Writer) {
	if len(t.Header) == 0 {
		return
	}
	n := len(t.Header)
	width := make([]int, n)
	for i, h := range t.Header {
		width[i] = DisplayWidth(h)
	}
	for _, row := range t.Rows {
		for i := 0; i < n && i < len(row); i++ {
			if d := DisplayWidth(row[i]); d > width[i] {
				width[i] = d
			}
		}
	}

	writeRow := func(cells []string) {
		var b strings.Builder
		for i := 0; i < n; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			pad := width[i] - DisplayWidth(cell)
			if pad < 0 {
				pad = 0
			}
			if t.Right != nil && t.Right[i] {
				b.WriteString(strings.Repeat(" ", pad))
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", pad))
			}
			if i < n-1 {
				b.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}

	writeRow(t.Header)
	sep := make([]string, n)
	for i := range sep {
		sep[i] = strings.Repeat("-", width[i])
	}
	writeRow(sep)
	if len(t.Rows) == 0 {
		empty := make([]string, n)
		empty[0] = "（空）"
		writeRow(empty)
		return
	}
	for _, row := range t.Rows {
		writeRow(row)
	}
}

// DisplayWidth 计算终端显示宽度：CJK / 全角字符占两格。
// 只处理常见区间——emoji 与组合字符不追求精确，列对齐够用即可。
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r == '\t':
			w += 4
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r):
			// 组合标记不占位
		case isWide(r):
			w += 2
		default:
			w++
		}
	}
	return w
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 朝鲜文字母
		r >= 0x2E80 && r <= 0x303E, // CJK 部首/标点
		r >= 0x3041 && r <= 0x33FF, // 假名、CJK 兼容
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 基本区
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, // 谚文
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60, // 全角
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // emoji（近似）
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// Trunc 按显示宽度截断并加省略号。表格里放完整 prompt 会把列撑爆。
func Trunc(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if DisplayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := 1
		if isWide(r) {
			rw = 2
		}
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// HumanSize 把字节数变成人看的单位。
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// Confirm 二次确认。默认不通过：危险操作的默认答案必须是「不做」。
func Confirm(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [y/N]: ", prompt)
	var line string
	if _, err := fmt.Fscanln(r, &line); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// ── 参数 ─────────────────────────────────────────────────────

// SplitFlags 把 args 拆成「flag 段」和「位置参数」。
//
// 为什么需要它：标准库 flag 遇到第一个非 flag 参数就停止解析，而我们的用法是
// `astra-cli run "echo hi" --wait`——位置参数在前、flag 在后。先切分再解析，
// 就能同时支持 `run --wait "echo hi"` 与 `run "echo hi" --wait`。
//
// takesValue 用来判断 `--k 8` 里的 8 是值还是位置参数：不告诉它，8 会被当命令。
func SplitFlags(args []string, takesValue map[string]bool) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			return flags, pos
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // --k=8 自带值
		}
		if takesValue[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, pos
}

// BoolFlags 生成 takesValue 的便捷构造：列出的都是「带值的」。
func BoolFlags(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
