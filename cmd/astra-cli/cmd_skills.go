package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

// skills 是随仓库分发的 agent 能力说明（skills/<name>/SKILL.md）。
//
// CLI 在这里只做三件事：列出来、打印出来、复制到本地目录。
// 真正的「怎么用」写在 SKILL.md 里——把提示词塞进 CLI 的代码里，
// 改一次措辞就要重新发版，而 skill 文件是用户可以直接读、直接改的。

// skillMeta 是 SKILL.md 的 frontmatter（见 platform-extension.md 第六节）。
type skillMeta struct {
	Name        string
	Description string
	Version     string
	Requires    []string
	Dir         string
}

const skillsHelp = `用法：
  astra-cli skills list [--from DIR]                          列出 skill
  astra-cli skills show <名称> [--from DIR]                    打印 SKILL.md
  astra-cli skills install <名称> [--dest DIR] [--from DIR] [--force]
                                                              安装到 ~/.astra/skills/<名称>
`

func (a *app) cmdSkills(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.skillsList(args)
	case "show", "cat":
		return a.skillsShow(args)
	case "install":
		return a.skillsInstall(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, skillsHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "skills 没有子命令 %q\n\n%s", sub, skillsHelp)
		return 2
	}
}

func (a *app) skillsList(args []string) int {
	fs := a.newFlagSet("skills list")
	from := fs.String("from", "", "skills 根目录（默认自动向上查找仓库里的 skills/）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "from"))
	if done {
		return code
	}
	root, err := findSkillsRoot(*from)
	if err != nil {
		return a.fail(err)
	}
	metas, err := readSkills(root)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(metas, func() error {
		t := clikit.Table{Header: []string{"NAME", "VERSION", "REQUIRES", "DESCRIPTION"}}
		for _, m := range metas {
			t.Rows = append(t.Rows, []string{
				m.Name,
				dash(m.Version),
				strings.Join(m.Requires, ","),
				clikit.Trunc(m.Description, 56),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n目录：%s\n", root)
		return nil
	})
}

func (a *app) skillsShow(args []string) int {
	fs := a.newFlagSet("skills show")
	from := fs.String("from", "", "skills 根目录")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "from"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli skills show <名称> [--from DIR]"); err != nil {
		return a.fail(err)
	}
	dir, err := resolveSkillDir(*from, pos[0])
	if err != nil {
		return a.fail(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprint(a.out, string(raw))
	if !strings.HasSuffix(string(raw), "\n") {
		fmt.Fprintln(a.out)
	}
	return 0
}

func (a *app) skillsInstall(args []string) int {
	fs := a.newFlagSet("skills install")
	from := fs.String("from", "", "skills 根目录或某个 skill 目录")
	dest := fs.String("dest", "", "目标目录（默认 ~/.astra/skills/<名称>）")
	force := fs.Bool("force", false, "目标已存在时覆盖")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "from", "dest"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli skills install <名称> [--dest DIR] [--from DIR] [--force]"); err != nil {
		return a.fail(err)
	}
	name := pos[0]
	src, err := resolveSkillDir(*from, name)
	if err != nil {
		return a.fail(err)
	}

	dst := clikit.ExpandHome(*dest)
	if dst == "" {
		base, err := clikit.Path("skills")
		if err != nil {
			return a.fail(err)
		}
		dst = filepath.Join(base, name)
	}
	if _, err := os.Stat(dst); err == nil {
		if !*force {
			return a.fail(&client.UseError{Msg: fmt.Sprintf("目标已存在：%s（加 --force 覆盖）", dst)})
		}
		// 只在明确 --force 时删；install 的目标目录应当是「整个 skill」的完整拷贝，
		// 留着上一次的残留文件会让「装的是哪个版本」说不清。
		if err := os.RemoveAll(dst); err != nil {
			return a.fail(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return a.fail(err)
	}
	// os.CopyFS 递归复制，省掉手写 WalkDir 的权限/符号链接细节。
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return a.fail(fmt.Errorf("复制 %s → %s 失败: %w", src, dst, err))
	}
	abs, _ := filepath.Abs(dst)
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]string{
			"name": name, "from": src, "installedTo": abs,
		}))
	}
	fmt.Fprintf(a.out, "已安装 %s → %s\n", name, abs)
	fmt.Fprintf(a.out, "提示：把这个路径加进 agent 的 skills 目录，或直接读 %s/SKILL.md\n", abs)
	return 0
}

// findSkillsRoot 找仓库里的 skills 目录。
//
// 从当前目录向上找（最多 8 层），命中标准是「目录里有子目录且子目录含 SKILL.md」。
// 不依赖 go.mod / git：用户可能在 dist/ 或任意子目录里跑 CLI。
func findSkillsRoot(explicit string) (string, error) {
	if explicit != "" {
		p := clikit.ExpandHome(explicit)
		if ok := isSkillsRoot(p); ok {
			return p, nil
		}
		return "", &client.UseError{Msg: fmt.Sprintf("%s 不是有效的 skills 目录（需要 <name>/SKILL.md）", p)}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "skills")
		if isSkillsRoot(cand) {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", &client.UseError{Msg: "找不到 skills 目录：请用 --from 指定（例如 --from ./skills）"}
}

func isSkillsRoot(p string) bool {
	ents, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, e.Name(), "SKILL.md")); err == nil {
			return true
		}
	}
	return false
}

// resolveSkillDir 定位某个 skill 的目录。--from 既可以是 skills 根目录，
// 也可以直接是某个 skill 目录（这样 `--from ./skills/astra-run` 也能用）。
func resolveSkillDir(from, name string) (string, error) {
	if from != "" {
		p := clikit.ExpandHome(from)
		if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err == nil {
			return p, nil
		}
		cand := filepath.Join(p, name)
		if _, err := os.Stat(filepath.Join(cand, "SKILL.md")); err != nil {
			return "", &client.UseError{Msg: fmt.Sprintf("%s 里没有 skill %q", p, name)}
		}
		return cand, nil
	}
	root, err := findSkillsRoot("")
	if err != nil {
		return "", err
	}
	cand := filepath.Join(root, name)
	if _, err := os.Stat(filepath.Join(cand, "SKILL.md")); err != nil {
		var names []string
		if metas, e := readSkills(root); e == nil {
			for _, m := range metas {
				names = append(names, m.Name)
			}
		}
		return "", &client.UseError{Msg: fmt.Sprintf("没有 skill %q（可用：%s）", name, strings.Join(names, ", "))}
	}
	return cand, nil
}

func readSkills(root string) ([]skillMeta, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []skillMeta
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		md, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			continue
		}
		m := parseFrontMatter(string(md))
		if m.Name == "" {
			m.Name = e.Name()
		}
		m.Dir = dir
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseFrontMatter 解析 --- 包裹的 key: value 块。
//
// 只认我们自己的格式（见设计文档第六节）：值是单行的，
// requires 写成 [astra-cli] 或 astra-cli,astra-cli2。不引 YAML 库——
// 为了四行键值对拉一个解析器，反而给「格式到底支持什么」制造歧义。
func parseFrontMatter(doc string) skillMeta {
	var m skillMeta
	sc := bufio.NewScanner(strings.NewReader(doc))
	in := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			if in {
				break
			}
			in = true
			continue
		}
		if !in {
			if line == "" {
				continue
			}
			break // 没有 frontmatter
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
	return m
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
