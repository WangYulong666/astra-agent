package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

// skills 是随仓库分发的 agent 能力说明（skills/<name>/SKILL.md）。
//
// CLI 在这里只做四件事：列出来、打印出来、校验 requires、复制到本地目录。
// 真正的「怎么用」写在 SKILL.md 里——把提示词塞进 CLI 的代码里，
// 改一次措辞就要重新发版，而 skill 文件是用户可以直接读、直接改的。

// skillMeta 是 SKILL.md 的 frontmatter（见 platform-extension.md 第六节）。
// Unavailable 非空说明该 skill 的 requires 在本机不满足，清单里仍会列出它。
type skillMeta struct {
	Name        string
	Description string
	Version     string
	Requires    []string
	Dir         string
	Unavailable string
}

const skillsHelp = `用法：
  astra-cli skills list [--from DIR]                          列出 skill（含不可用原因）
  astra-cli skills show <名称> [--from DIR]                    打印 SKILL.md
  astra-cli skills check [--from DIR]                          校验 requires，存在不可用项退出码 1
  astra-cli skills install <名称> [--dest DIR] [--from DIR] [--force]
                                                              安装到 ~/.astra/skills/<名称>

默认目录：SKILL_DIR 环境变量 > ~/.astra/skills > 仓库 skills/（向上 8 层）
astra-cli 校验：ASTRA_CLI_BIN 指向可执行文件，或在 PATH 里能 LookPath 到 astra-cli
`

func (a *app) cmdSkills(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.skillsList(args)
	case "show", "cat":
		return a.skillsShow(args)
	case "check":
		return a.skillsCheck(args)
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
	from := fs.String("from", "", "skills 根目录（默认 SKILL_DIR / ~/.astra/skills / 仓库 skills/）")
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
		t := clikit.Table{Header: []string{"NAME", "VERSION", "REQUIRES", "UNAVAILABLE", "DESCRIPTION"}}
		for _, m := range metas {
			t.Rows = append(t.Rows, []string{
				m.Name,
				dash(m.Version),
				strings.Join(m.Requires, ","),
				dash(m.Unavailable),
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
	// 提示走 stderr：stdout 必须是干净的 SKILL.md 全文，能直接 `skills show x > x.md`
	// 重定向使用，警告混进去会污染落地文件。
	meta := parseFrontMatter(string(raw))
	if why := cliAvailability(meta.Requires); why != "" {
		fmt.Fprintf(a.err, "警告：skill %q 当前不可用：%s\n", pos[0], why)
	}
	fmt.Fprint(a.out, string(raw))
	if !strings.HasSuffix(string(raw), "\n") {
		fmt.Fprintln(a.out)
	}
	return 0
}

// skillsCheck 校验每个 skill 的 requires，列出不可用项。
// 退出码：0 全部可用；1 存在不可用项；2 用法/读目录错误。
func (a *app) skillsCheck(args []string) int {
	fs := a.newFlagSet("skills check")
	from := fs.String("from", "", "skills 根目录")
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
	var bad []skillMeta
	for _, m := range metas {
		if m.Unavailable != "" {
			bad = append(bad, m)
		}
	}

	type result struct {
		Root        string      `json:"root"`
		Checked     int         `json:"checked"`
		Unavailable []skillMeta `json:"unavailable"`
	}
	res := result{Root: root, Checked: len(metas), Unavailable: bad}
	if a.g.json {
		if err := clikit.PrintJSON(a.out, res); err != nil {
			return a.fail(err)
		}
	} else {
		if len(bad) == 0 {
			fmt.Fprintf(a.out, "全部 %d 个 skill 的 requires 均满足。\n", len(metas))
		} else {
			fmt.Fprintf(a.out, "检查了 %d 个 skill，其中 %d 个不可用：\n", len(metas), len(bad))
			for _, m := range bad {
				fmt.Fprintf(a.out, "  - %s：%s\n", m.Name, m.Unavailable)
			}
		}
		fmt.Fprintf(a.out, "目录：%s\n", root)
	}
	if len(bad) > 0 {
		return 1
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

// findSkillsRoot 定位统一目录：SKILL_DIR 环境变量 > ~/.astra/skills > 仓库 skills/。
//
// 前两者是「目录统一」后的权威位置：distill.Accept 与 skillhub 的索引都落在
// 那里，CLI 必须先看它们，否则会列出一份和运行时不一样的旧清单。
// 只有这两个都没建成时才回退到从当前目录向上翻仓库 skills/（保留开发体验）：
// 从源码树里跑 CLI、又没启过服务端时，至少还能看到内置的那三个。
func findSkillsRoot(explicit string) (string, error) {
	if explicit != "" {
		p := clikit.ExpandHome(explicit)
		if ok := isSkillsRoot(p); ok {
			return p, nil
		}
		return "", &client.UseError{Msg: fmt.Sprintf("%s 不是有效的 skills 目录（需要 <name>/SKILL.md）", p)}
	}
	if p := strings.TrimSpace(os.Getenv("SKILL_DIR")); p != "" {
		p = clikit.ExpandHome(p)
		if isSkillsRoot(p) {
			return p, nil
		}
	}
	if base, err := clikit.Path("skills"); err == nil && isSkillsRoot(base) {
		return base, nil
	}

	// 回退：从当前目录向上找（最多 8 层），不依赖 go.mod / git。
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
	return "", &client.UseError{Msg: "找不到 skills 目录：请设置 SKILL_DIR，或用 --from 指定"}
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
		m.Unavailable = cliAvailability(m.Requires)
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

// cliAvailability 按 skillhub 的同一套规则判定 requires，返回 "" 表示可用。
//
// astra-cli 用 ASTRA_CLI_BIN / exec.LookPath 在本机确认；其余 requires 是
// 服务端 /v1/meta 的能力开关名，CLI 本地拿不到开关状态，保守地标为不可用
// 并写清原因——宁可让运营看见一条待确认，也不要假装能用。
func cliAvailability(requires []string) string {
	for _, r := range requires {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if r == "astra-cli" {
			if !astraCLIExecutable() {
				return "astra-cli 不可用"
			}
			continue
		}
		return fmt.Sprintf("能力 %s 未启用（CLI 无开关信息，请用服务端 /v1/skills 复核）", r)
	}
	return ""
}

func astraCLIExecutable() bool {
	if bin := strings.TrimSpace(os.Getenv("ASTRA_CLI_BIN")); bin != "" {
		info, err := os.Stat(clikit.ExpandHome(bin))
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	// LookPath 本身就要求文件有执行位，这里再 Stat 一次只是把语义写死，
	// 免得将来换平台时 LookPath 的行为差异悄悄放宽校验。
	p, err := exec.LookPath("astra-cli")
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
