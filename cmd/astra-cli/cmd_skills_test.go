package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSkillMD = `---
name: %s
description: %s
version: 1
requires: [%s]
---
# %s

正文。
`

func skillBody(name, desc, requires string) string {
	return fmt.Sprintf(testSkillMD, name, desc, requires, name)
}

func makeSkillsRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func cleanHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func noCLI(t *testing.T) {
	t.Helper()
	t.Setenv("ASTRA_CLI_BIN", filepath.Join(t.TempDir(), "definitely-not-a-cli"))
}

func yesCLI(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bin", "astra-cli")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASTRA_CLI_BIN", p)
}

// ── 1. 目录优先级 ────────────────────────────────────────────

func TestFindSkillsRootPriority(t *testing.T) {
	envRoot := makeSkillsRoot(t, map[string]string{
		"from-env": skillBody("from-env", "环境变量目录", ""),
	})
	home := t.TempDir()
	homeSkills := filepath.Join(home, ".astra", "skills")
	if err := os.MkdirAll(filepath.Join(homeSkills, "from-home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeSkills, "from-home", "SKILL.md"),
		[]byte(skillBody("from-home", "HOME 目录", "")), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("SKILL_DIR 优先", func(t *testing.T) {
		t.Setenv("HOME", home)
		t.Setenv("SKILL_DIR", envRoot)
		got, err := findSkillsRoot("")
		if err != nil {
			t.Fatal(err)
		}
		if got != envRoot {
			t.Fatalf("got %q，期望 SKILL_DIR=%q", got, envRoot)
		}
	})

	t.Run("HOME 次之", func(t *testing.T) {
		t.Setenv("HOME", home)
		t.Setenv("SKILL_DIR", "")
		got, err := findSkillsRoot("")
		if err != nil {
			t.Fatal(err)
		}
		if got != homeSkills {
			t.Fatalf("got %q，期望 %q", got, homeSkills)
		}
	})

	t.Run("仓库 skills 回退", func(t *testing.T) {
		cleanHome(t)
		t.Setenv("SKILL_DIR", "")
		got, err := findSkillsRoot("")
		if err != nil {
			t.Fatal(err)
		}
		if !isSkillsRoot(got) {
			t.Fatalf("回退目录 %q 不是有效 skills 根目录", got)
		}
		if _, err := os.Stat(filepath.Join(got, "astra-run", "SKILL.md")); err != nil {
			t.Fatalf("回退目录里没有内置 astra-run: %v", err)
		}
	})

	t.Run("--from 最优先", func(t *testing.T) {
		t.Setenv("HOME", home)
		t.Setenv("SKILL_DIR", envRoot)
		got, err := findSkillsRoot(homeSkills)
		if err != nil {
			t.Fatal(err)
		}
		if got != homeSkills {
			t.Fatalf("got %q，期望显式 --from=%q", got, homeSkills)
		}
	})
}

// ── 2. requires 判定 ─────────────────────────────────────────

func TestCLIRequiresAvailability(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-cli":    skillBody("needs-cli", "需要 CLI", "astra-cli"),
		"needs-switch": skillBody("needs-switch", "需要开关", "vision"),
		"plain":        skillBody("plain", "无依赖", ""),
	})

	noCLI(t)
	metas, err := readSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]skillMeta{}
	for _, m := range metas {
		byName[m.Name] = m
	}
	if got := byName["needs-cli"].Unavailable; got != "astra-cli 不可用" {
		t.Errorf("needs-cli = %q", got)
	}
	if got := byName["needs-switch"].Unavailable; !strings.Contains(got, "能力 vision 未启用") {
		t.Errorf("needs-switch = %q", got)
	}
	if got := byName["plain"].Unavailable; got != "" {
		t.Errorf("plain 不该不可用: %q", got)
	}

	yesCLI(t)
	metas, _ = readSkills(root)
	for _, m := range metas {
		if m.Name == "needs-cli" && m.Unavailable != "" {
			t.Errorf("ASTRA_CLI_BIN 指向可执行文件后仍不可用: %q", m.Unavailable)
		}
	}
}

func TestParseFrontMatterAndAvailabilityHelpers(t *testing.T) {
	m := parseFrontMatter(skillBody("x", "描述里带: 冒号", "astra-cli, vision"))
	if m.Name != "x" || m.Version != "1" {
		t.Fatalf("frontmatter = %+v", m)
	}
	if !strings.HasPrefix(m.Description, "描述里带") {
		t.Errorf("description = %q", m.Description)
	}
	if len(m.Requires) != 2 || m.Requires[0] != "astra-cli" || m.Requires[1] != "vision" {
		t.Errorf("requires = %v", m.Requires)
	}
	if why := cliAvailability(nil); why != "" {
		t.Errorf("空 requires 不该不可用: %q", why)
	}
}

// ── 3. list 的 UNAVAILABLE 列 ────────────────────────────────

func TestSkillsListShowsUnavailableColumn(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-cli": skillBody("needs-cli", "需要 CLI", "astra-cli"),
	})
	cleanHome(t)
	noCLI(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"skills", "list", "--from", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "UNAVAILABLE") {
		t.Errorf("list 没有 UNAVAILABLE 列:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "astra-cli 不可用") {
		t.Errorf("list 没显示不可用原因:\n%s", out.String())
	}
}

func TestSkillsListJSONIncludesUnavailable(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-cli": skillBody("needs-cli", "需要 CLI", "astra-cli"),
	})
	cleanHome(t)
	noCLI(t)
	var out, errBuf bytes.Buffer
	if code := run([]string{"skills", "list", "--from", root, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	var metas []map[string]any
	if err := json.Unmarshal(out.Bytes(), &metas); err != nil {
		t.Fatalf("--json 不是合法 JSON: %v\n%s", err, out.String())
	}
	if len(metas) != 1 || metas[0]["Unavailable"] != "astra-cli 不可用" {
		t.Fatalf("--json = %v", metas)
	}
}

// ── 4. show 的不可用提示 ─────────────────────────────────────

func TestSkillsShowWarnsUnavailableButKeepsStdoutClean(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-cli": skillBody("needs-cli", "需要 CLI", "astra-cli"),
	})
	cleanHome(t)
	noCLI(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"skills", "show", "needs-cli", "--from", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "不可用") {
		t.Errorf("show 没显式提示不可用: %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "name: needs-cli") {
		t.Errorf("stdout 不是 SKILL.md 全文: %q", out.String())
	}
	if strings.Contains(out.String(), "不可用") {
		t.Error("警告污染了 stdout，重定向会写坏 SKILL.md")
	}
}

// ── 5. check 子命令 ──────────────────────────────────────────

func TestSkillsCheckExitCodes(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-cli": skillBody("needs-cli", "需要 CLI", "astra-cli"),
		"plain":     skillBody("plain", "无依赖", ""),
	})

	t.Run("存在不可用项退出 1", func(t *testing.T) {
		cleanHome(t)
		noCLI(t)
		var out, errBuf bytes.Buffer
		code := run([]string{"skills", "check", "--from", root}, &out, &errBuf)
		if code != 1 {
			t.Fatalf("exit = %d，期望 1；out=%s err=%s", code, out.String(), errBuf.String())
		}
		if !strings.Contains(out.String(), "needs-cli") || !strings.Contains(out.String(), "不可用") {
			t.Errorf("check 没列出不可用项:\n%s", out.String())
		}
	})

	t.Run("全部可用退出 0", func(t *testing.T) {
		cleanHome(t)
		yesCLI(t)
		var out, errBuf bytes.Buffer
		code := run([]string{"skills", "check", "--from", root}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("exit = %d，期望 0；out=%s err=%s", code, out.String(), errBuf.String())
		}
		if !strings.Contains(out.String(), "均满足") {
			t.Errorf("check 成功输出不对:\n%s", out.String())
		}
	})

	t.Run("--json 可解析", func(t *testing.T) {
		cleanHome(t)
		noCLI(t)
		var out, errBuf bytes.Buffer
		code := run([]string{"skills", "check", "--from", root, "--json"}, &out, &errBuf)
		if code != 1 {
			t.Fatalf("exit = %d，期望 1；err=%s", code, errBuf.String())
		}
		var res struct {
			Root        string           `json:"root"`
			Checked     int              `json:"checked"`
			Unavailable []map[string]any `json:"unavailable"`
		}
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("--json 不是合法 JSON: %v\n%s", err, out.String())
		}
		if res.Checked != 2 || len(res.Unavailable) != 1 || res.Unavailable[0]["Name"] != "needs-cli" {
			t.Fatalf("--json = %+v", res)
		}
	})
}

func TestSkillsHelpMentionsCheck(t *testing.T) {
	if !strings.Contains(skillsHelp, "skills check") || !strings.Contains(skillsHelp, "SKILL_DIR") {
		t.Fatalf("skillsHelp 没写 check / SKILL_DIR:\n%s", skillsHelp)
	}
}

// 能力开关（无本地开关信息）只能警告，不能让 skills check 假失败退出 1。
func TestSkillsCheckSwitchRequiresWarnsOnly(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"needs-switch": skillBody("needs-switch", "需要开关", "vision"),
		"plain":        skillBody("plain", "无依赖", ""),
	})
	cleanHome(t)
	noCLI(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"skills", "check", "--from", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("能力开关只应警告、不该退出 1；实际 %d（out=%s err=%s）", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "警告") || !strings.Contains(out.String(), "vision") {
		t.Fatalf("没打印能力开关警告:\n%s", out.String())
	}
}

// 主 help 必须列出 skills check（以前漏列）。
func TestMainHelpMentionsSkillsCheck(t *testing.T) {
	var out, errBuf bytes.Buffer
	run([]string{"--help"}, &out, &errBuf)
	if !strings.Contains(out.String(), "skills check") {
		t.Fatalf("主 help 没列出 skills check:\n%s", out.String())
	}
}

// ── 6. install：名称校验 / 路径穿越 / 源校验 / 正常安装 ──────

func TestValidateSkillName(t *testing.T) {
	valid := []string{"astra-run", "a", "a1", "abc-123-def", "x-y-z"}
	invalid := []string{"", "../escape", "/tmp/absolute", "a/b", "UPPER", "a_b", "a b", "-lead", "trail-", "a--b", ".."}
	for _, n := range valid {
		if err := validateSkillName(n); err != nil {
			t.Errorf("合法名称 %q 被拒: %v", n, err)
		}
	}
	for _, n := range invalid {
		if err := validateSkillName(n); err == nil {
			t.Errorf("非法名称 %q 没被拒", n)
		}
	}
}

func TestJoinInBaseRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := joinInBase(base, "ok-name"); err != nil {
		t.Fatalf("合法拼接受阻: %v", err)
	}
	for _, bad := range []string{"../escape", "../../x", "a/../../b"} {
		if _, err := joinInBase(base, bad); err == nil {
			t.Errorf("joinInBase 没挡住 %q", bad)
		}
	}
}

func TestValidateSkillSource(t *testing.T) {
	t.Run("没有 SKILL.md", func(t *testing.T) {
		if err := validateSkillSource(t.TempDir()); err == nil {
			t.Fatal("应拒绝无 SKILL.md 的源")
		}
	})
	t.Run("SKILL.md 超限", func(t *testing.T) {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(strings.Repeat("x", maxSkillFileBytes+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateSkillSource(d); err == nil {
			t.Fatal("应拒绝超 256KB 的 SKILL.md")
		}
	})
	t.Run("正常", func(t *testing.T) {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: demo\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateSkillSource(d); err != nil {
			t.Fatalf("正常源被拒: %v", err)
		}
	})
}

func TestSkillsInstallRejectsInvalidNames(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"demo": skillBody("demo", "演示", ""),
	})
	for _, name := range []string{"../escape", "/tmp/astra-install-absolute", ""} {
		t.Run(fmt.Sprintf("name=%q", name), func(t *testing.T) {
			destParent := t.TempDir()
			var out, errBuf bytes.Buffer
			code := run([]string{"skills", "install", name, "--from", root, "--dest", filepath.Join(destParent, "dst")}, &out, &errBuf)
			if code != 2 {
				t.Fatalf("非法名称 %q 应返回用法错误 2，实际 %d（out=%s err=%s）", name, code, out.String(), errBuf.String())
			}
			// 一旦漏过校验，../escape 会写到目标外；确认没有逃逸文件。
			if _, err := os.Stat(filepath.Join(destParent, "escape", "SKILL.md")); err == nil {
				t.Fatal("发生路径穿越：在目标外创建了文件")
			}
		})
	}
}

func TestSkillsInstallOK(t *testing.T) {
	root := makeSkillsRoot(t, map[string]string{
		"demo": skillBody("demo", "演示", ""),
	})

	t.Run("默认目标跟随 SKILL_DIR", func(t *testing.T) {
		destBase := t.TempDir()
		cleanHome(t)
		t.Setenv("SKILL_DIR", destBase)
		var out, errBuf bytes.Buffer
		code := run([]string{"skills", "install", "demo", "--from", root}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
		}
		raw, err := os.ReadFile(filepath.Join(destBase, "demo", "SKILL.md"))
		if err != nil {
			t.Fatalf("没安装到 SKILL_DIR: %v", err)
		}
		if !strings.Contains(string(raw), "name: demo") {
			t.Fatalf("安装内容不对: %q", string(raw))
		}
	})

	t.Run("显式 --dest", func(t *testing.T) {
		cleanHome(t)
		t.Setenv("SKILL_DIR", "")
		dest := filepath.Join(t.TempDir(), "custom")
		var out, errBuf bytes.Buffer
		code := run([]string{"skills", "install", "demo", "--from", root, "--dest", dest}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, errBuf.String())
		}
		if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
			t.Fatalf("--dest 没生效: %v", err)
		}
	})
}
