package clikit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 防漂移：登记表是手抄的，手抄就可能漏。这里直接读两个 CLI 的源码，
// 把 `case "..."` 里的命令词抠出来，逐个回查 KnownCommands()。
// 新增子命令却忘了登记 → 这里红，而不是等能力清单悄悄少一项。
//
// 不追求完美解析（不用 go/parser）：够覆盖 `case "tail", "list", "ls":`
// 和 `case "memory":` 这类常见写法即可。解析不到或多解出来的，见下方显式跳过表。

var (
	// caseRe 抓一条 case 子句里所有字符串字面量，允许跨行
	// （admin-cli/cmd_logs.go 的 isLogsSub 是一条跨两行的多别名 case）。
	caseRe = regexp.MustCompile(`(?m)^[ \t]*case[ \t]+((?:"[^"]*"\s*,?\s*)+):`)
	wordRe = regexp.MustCompile(`"([^"]*)"`)
)

// nonCommandCases 是源码里 `case "xxx"` 但不是命令分派的词：
//   - astra-cli/cmd_run.go：run 终态 "completed"/"cancelled"，
//     streamPrinter 里的状态比较 "waiting_approval"（若写成 case 也一并兜住）；
//   - astra-cli/cmd_skills.go：解析 SKILL.md frontmatter 的键
//     "name"/"description"/"version"/"requires"。
//
// 它们出现在 cmd_*.go 里，按「分组子命令」提取会误报，故显式跳过并注明。
// 拼接 / 动态生成、正则抠不出来的命令（本仓库目前没有）也在这里登记跳过。
var nonCommandCases = map[string]bool{
	"completed": true, "cancelled": true, "waiting_approval": true,
	"name": true, "description": true, "version": true, "requires": true,
}

// allowedRequires 是能力开关白名单，与设计文档一致。
var allowedRequires = map[string]bool{
	"memory": true, "trace": true, "egress": true, "connectors": true,
	"scheduler": true, "distill": true, "browser": true, "collab": true,
	"activity": true, "capabilities": true, "skills": true, "insights": true,
}

func isMetaWord(w string) bool {
	// 空串是 case ""（无子命令/help 分支）；-h/--help/--version 是 flag；
	// help/default 是分派表里的元项，都不是需要登记的原子命令。
	return w == "" || strings.HasPrefix(w, "-") || w == "help" || w == "default"
}

func extractCaseClauses(src string) [][]string {
	var out [][]string
	for _, m := range caseRe.FindAllStringSubmatch(src, -1) {
		var words []string
		for _, sm := range wordRe.FindAllStringSubmatch(m[1], -1) {
			words = append(words, sm[1])
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

func readCLISources(t *testing.T, cli string) map[string]string {
	t.Helper()
	// go test 的 CWD 恒为被测包目录（internal/client/clikit），上溯三层到仓库根。
	dir := filepath.Join("..", "..", "..", "cmd", cli)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s 源码目录失败：%v", cli, err)
	}
	files := map[string]string{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name != "main.go" && !strings.HasPrefix(name, "cmd_") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 %s/%s 失败：%v", cli, name, err)
		}
		files[name] = string(data)
	}
	if _, ok := files["main.go"]; !ok {
		t.Fatalf("%s 没读到 main.go", cli)
	}
	return files
}

func TestCLIRegistryCoversCommands(t *testing.T) {
	for _, cli := range []string{"astra-cli", "admin-cli"} {
		t.Run(cli, func(t *testing.T) { checkCLICoverage(t, cli) })
	}
}

func checkCLICoverage(t *testing.T, cli string) {
	t.Helper()

	// 先按 CLI 建三张索引：
	//   registered  -> 全量 Name
	//   topLevel    -> 单词命令（whoami / version ...）
	//   groupNames  -> 多词命令的第一段（memory / logs / tenant ...）
	registered := map[string]bool{}
	topLevel := map[string]bool{}
	groupNames := map[string]bool{}
	count := 0
	for _, c := range KnownCommands() {
		if c.CLI != cli {
			continue
		}
		count++
		registered[c.Name] = true
		if i := strings.IndexByte(c.Name, ' '); i >= 0 {
			groupNames[c.Name[:i]] = true
		} else {
			topLevel[c.Name] = true
		}
	}
	if count == 0 {
		t.Fatalf("%s 一条命令都没登记", cli)
	}

	files := readCLISources(t, cli)
	mainWords := map[string]bool{} // main.go 顶层分派里出现过的词

	for name, src := range files {
		clauses := extractCaseClauses(src)

		if name == "main.go" {
			// 顶层分派：每个非元项都必须是「已登记单词命令」或「已登记分组」。
			for _, words := range clauses {
				for _, w := range words {
					if isMetaWord(w) {
						continue
					}
					mainWords[w] = true
					if !topLevel[w] && !groupNames[w] {
						t.Errorf("%s/main.go: 顶层命令 %q 未在 KnownCommands() 登记（新增命令忘了补表？）", cli, w)
					}
				}
			}
			continue
		}

		// cmd_<group>.go：case 行里的词是子命令（规范名 + 别名）。
		group := strings.TrimSuffix(strings.TrimPrefix(name, "cmd_"), ".go")
		for _, words := range clauses {
			covered := false
			for _, w := range words {
				if registered[group+" "+w] {
					covered = true
					break
				}
			}
			if covered {
				// 该 case 行至少含一个已登记的规范子命令，其余词视为别名
				// （ls / find / log / outbound ...），无需一一登记。
				continue
			}
			for _, w := range words {
				if w == "" || nonCommandCases[w] {
					continue
				}
				t.Errorf("%s/%s: 子命令 %q（完整名 %q）未登记；新增子命令请同步 KnownCommands()",
					cli, name, w, group+" "+w)
			}
		}
	}

	// 反向：登记表中的单词命令 / 分组，必须真的在 main.go 的分派里出现，
	// 防止登记了拼错的名字、或命令删了但表没删（残留）。
	for w := range topLevel {
		if !mainWords[w] {
			t.Errorf("%s: 登记了顶层命令 %q，但 main.go 里没有对应 case", cli, w)
		}
	}
	for g := range groupNames {
		if !mainWords[g] {
			t.Errorf("%s: 登记了分组 %q，但 main.go 里没有对应 case", cli, g)
		}
	}
}

func TestKnownCommandsIntegrity(t *testing.T) {
	cmds := KnownCommands()
	seen := map[string]bool{}
	for i, c := range cmds {
		if c.CLI != "astra-cli" && c.CLI != "admin-cli" {
			t.Errorf("[%d] 非法 CLI=%q", i, c.CLI)
		}
		key := c.CLI + "\x00" + c.Name
		if seen[key] {
			t.Errorf("[%d] 重复登记 CLI+Name=%q/%q", i, c.CLI, c.Name)
		}
		seen[key] = true
		if c.Name == "" || strings.TrimSpace(c.Name) != c.Name {
			t.Errorf("[%d] Name 为空或带首尾空格：%q", i, c.Name)
		}
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("[%d] %q 的 Description 为空", i, c.Name)
		}
		if strings.TrimSpace(c.Usage) == "" {
			t.Errorf("[%d] %q 的 Usage 为空", i, c.Name)
		}
		for _, r := range c.Requires {
			if !allowedRequires[r] {
				t.Errorf("[%d] %q 的 Requires 含非法开关 %q", i, c.Name, r)
			}
		}
	}
}
