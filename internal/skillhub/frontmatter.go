package skillhub

import (
	"bufio"
	"errors"
	"regexp"
	"strings"
)

// frontMatter 是 SKILL.md 顶部 --- 块里的四个字段。
// 解析思路照搬 cmd/astra-cli/cmd_skills.go：只认单行的 key: value，
// 不引 YAML 库（四行键值对不值得，反而让「支持什么格式」变得含糊）。
type frontMatter struct {
	Name        string
	Description string
	Version     string
	Requires    []string
}

// validNameRe 与 distill 的 slug 规则一致：小写字母/数字/连字符，
// 且连字符不能开头、结尾或连续。名字会进索引、也可能被别人拿去拼路径，
// 用最严的那一种。
var validNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validName(s string) bool { return validNameRe.MatchString(s) }

// parseFrontMatter 解析 frontmatter。返回 error 表示**结构**坏了
// （缺 --- 块 / 没闭合）；缺 version、缺 requires 这类「字段没写」不算错。
//
// 与 CLI 的宽松版不同：CLI 读不到名字就退回目录名，这里必须把坏文件
// 标成 unavailable 留在清单里，不能静默丢掉（运营要能看见）。
func parseFrontMatter(doc string) (frontMatter, error) {
	var m frontMatter
	sc := bufio.NewScanner(strings.NewReader(doc))
	in, closed, seen := false, false, false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			if in {
				closed = true
				break
			}
			in, seen = true, true
			continue
		}
		if !in {
			if line == "" {
				continue
			}
			break // 正文开头，没有 frontmatter
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "name":
			m.Name = trimQuotes(v)
		case "description":
			m.Description = trimQuotes(v)
		case "version":
			m.Version = trimQuotes(v)
		case "requires":
			v = strings.TrimSuffix(strings.TrimPrefix(trimQuotes(v), "["), "]")
			for _, part := range strings.Split(v, ",") {
				if p := strings.TrimSpace(part); p != "" {
					m.Requires = append(m.Requires, p)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !seen {
		return m, errors.New("缺少 --- frontmatter 块")
	}
	if !closed {
		return m, errors.New("frontmatter 没有闭合 ---")
	}
	return m, nil
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// ensureRequires 让 body 的 frontmatter 里的 requires 与入参一致。
//
// Publish 把 requires 单独当参数传进来（调用方不必自己拼 YAML），但落盘的
// SKILL.md 必须是自描述的：索引随时可能被人删掉重建，那时候唯一的事实
// 来源只有文件本身。已经一致时原样返回，不改动正文一个字节。
func ensureRequires(body string, requires []string) string {
	want := "requires: [" + strings.Join(requires, ", ") + "]"
	lines := strings.Split(body, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) != "---" {
				continue
			}
			for j := 1; j < i; j++ {
				k, _, ok := strings.Cut(lines[j], ":")
				if ok && strings.TrimSpace(k) == "requires" {
					if strings.TrimSpace(lines[j]) == want {
						return body
					}
					lines[j] = want
					return strings.Join(lines, "\n")
				}
			}
			// 没有 requires 行：插在闭合 --- 之前（frontmatter 末尾）。
			merged := make([]string, 0, len(lines)+1)
			merged = append(merged, lines[:i]...)
			merged = append(merged, want)
			merged = append(merged, lines[i:]...)
			return strings.Join(merged, "\n")
		}
	}
	// 没有合法 frontmatter：补一个最小块。name 缺省时由目录名兜底，
	// 不在这里硬塞，避免和正文里已有的名字冲突。
	head := []string{"---", "version: 1", want, "---", ""}
	return strings.Join(append(head, lines...), "\n")
}
