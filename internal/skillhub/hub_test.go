package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

const skillDoc = `---
name: %s
description: %s
version: 1
%s---
# 正文

步骤写在这里。
`

func doc(name, desc, requires string) string {
	r := ""
	if requires != "" {
		r = "requires: [" + requires + "]\n"
	}
	return fmt.Sprintf(skillDoc, name, desc, r)
}

// writeSkill 直接往磁盘放一个 SKILL.md，刻意绕过 Hub 的所有方法——
// 这正是 distill.Accept 的行为（只写文件、不发通知）。
func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "SKILL.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func execBin(t *testing.T) string {
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
	return p
}

func openHub(t *testing.T, cfg Config, bus *eventbus.Hub) Hub {
	t.Helper()
	h, err := Open(cfg, bus)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return h
}

func findSkill(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// ── 1. 内置同步 ──────────────────────────────────────────────

func TestOpenSyncsBuiltinsWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	custom := doc("astra-run", "人手改过的版本", "astra-cli")
	writeSkill(t, dir, "astra-run", custom)

	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	_ = h

	got, err := os.ReadFile(filepath.Join(dir, "astra-run", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Fatal("内置同步覆盖了人手改过的 SKILL.md")
	}
	for _, name := range []string{"astra-memory", "astra-files"} {
		if _, err := os.Stat(filepath.Join(dir, name, "SKILL.md")); err != nil {
			t.Errorf("内置 %s 没同步进 SkillDir: %v", name, err)
		}
	}
	// 重开一次（模拟进程重启）仍然不覆盖。
	if _, err := Open(Config{SkillDir: dir, CLIBin: execBin(t)}, nil); err != nil {
		t.Fatalf("重开: %v", err)
	}
	got2, _ := os.ReadFile(filepath.Join(dir, "astra-run", "SKILL.md"))
	if string(got2) != custom {
		t.Fatal("重启后内置同步覆盖了手改文件")
	}
}

// ── 2. 索引格式 / 原子写 ─────────────────────────────────────

func TestIndexAtomicWriteAndShape(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, indexFileName)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("index.json 不存在: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("index.json 权限 = %o，期望 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var idx indexFile
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("index.json 不是合法 JSON: %v\n%s", err, raw)
	}
	if idx.Version != indexFileVersion || len(idx.Skills) != 3 {
		t.Fatalf("索引 = %+v", idx)
	}
	for _, e := range idx.Skills {
		if e.Name == "" || e.Description == "" || e.Version == "" || e.Source == "" ||
			e.Status == "" || e.UpdatedAt.IsZero() || len(e.Requires) == 0 {
			t.Errorf("索引条目字段不全: %+v", e)
		}
	}
	// 原子写不能留下 .tmp-* 半成品。
	left, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if len(left) != 0 {
		t.Errorf("残留临时文件: %v", left)
	}
}

// ── 3. List 懒刷新（模拟 distill.Accept 只写文件） ────────────

func TestListAutoDiscoversNewFileAndContentChange(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	ctx := context.Background()

	before, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("初始清单 = %d，期望 3 个内置", len(before))
	}

	// 直接落盘，不调用 Hub 的任何方法：等价于 distill.Accept。
	writeSkill(t, dir, "brand-new", doc("brand-new", "第一版描述", ""))
	after, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := findSkill(after, "brand-new")
	if !ok {
		t.Fatalf("List 没发现新文件: %+v", after)
	}
	if s.Source != "distilled" || s.Status != "ready" || s.Description != "第一版描述" {
		t.Errorf("新 skill = %+v", s)
	}

	// 覆盖同一文件（内容变了但名字没变）也要被发现：靠全文比对而非只看新增。
	writeSkill(t, dir, "brand-new", doc("brand-new", "改过的描述", ""))
	changed, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := findSkill(changed, "brand-new")
	if s2.Description != "改过的描述" {
		t.Errorf("SKILL.md 变更没被 List 发现: %+v", s2)
	}

	// 删除也要反映出来。
	if err := os.RemoveAll(filepath.Join(dir, "brand-new")); err != nil {
		t.Fatal(err)
	}
	deleted, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findSkill(deleted, "brand-new"); ok {
		t.Error("删掉的 skill 还在清单里")
	}
}

func TestListRebuildsMissingIndex(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	if err := os.Remove(filepath.Join(dir, indexFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, indexFileName)); err != nil {
		t.Fatalf("List 没把 index.json 重建回来: %v", err)
	}
}

// ── 4. requires 校验 ─────────────────────────────────────────

func TestRequiresAstraCLI(t *testing.T) {
	dir := t.TempDir()
	// CLIBin 为空 → 三个内置（都 requires: [astra-cli]）全部 unavailable。
	noCLI := openHub(t, Config{SkillDir: dir}, nil)
	list, err := noCLI.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Status != "unavailable" || s.Unavailable != "astra-cli 不可用" {
			t.Errorf("%s = %+v，期望 astra-cli 不可用", s.Name, s)
		}
	}

	// 指向可执行文件 → ready。
	dir2 := t.TempDir()
	withCLI := openHub(t, Config{SkillDir: dir2, CLIBin: execBin(t)}, nil)
	list2, err := withCLI.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list2 {
		if s.Status != "ready" || s.Unavailable != "" {
			t.Errorf("%s = %+v，期望 ready", s.Name, s)
		}
	}

	// 指向目录不算可执行。
	dir3 := t.TempDir()
	dirBin := t.TempDir()
	bad := openHub(t, Config{SkillDir: dir3, CLIBin: dirBin}, nil)
	list3, _ := bad.List(context.Background())
	if list3[0].Status != "unavailable" {
		t.Errorf("CLIBin 指向目录却被当成可用: %+v", list3[0])
	}
}

func TestRequiresCapabilitySwitch(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "needs-switch", doc("needs-switch", "需要开关", "vision"))

	off := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t), Switches: map[string]bool{"vision": false}}, nil)
	list, err := off.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, ok := findSkill(list, "needs-switch")
	if !ok || s.Status != "unavailable" || s.Unavailable != "能力 vision 未启用" {
		t.Fatalf("开关关闭时 = %+v (ok=%v)", s, ok)
	}
	// Get 同样带上原因。
	g, err := off.Get(context.Background(), "needs-switch")
	if err != nil || g.Unavailable != "能力 vision 未启用" {
		t.Fatalf("Get = %+v (err=%v)", g, err)
	}

	dir2 := t.TempDir()
	writeSkill(t, dir2, "needs-switch", doc("needs-switch", "需要开关", "vision"))
	on := openHub(t, Config{SkillDir: dir2, CLIBin: execBin(t), Switches: map[string]bool{"vision": true}}, nil)
	list2, _ := on.List(context.Background())
	s2, _ := findSkill(list2, "needs-switch")
	if s2.Status != "ready" {
		t.Errorf("开关打开时 = %+v，期望 ready", s2)
	}
}

// ── 5. 坏文件不静默丢掉 ──────────────────────────────────────

func TestInvalidFrontmatterAndNameAreKept(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "no-frontmatter", "# 只有正文\n\n没有 --- 块。\n")
	writeSkill(t, dir, "unclosed", "---\nname: unclosed\ndescription: x\n")
	writeSkill(t, dir, "bad-name", doc("Bad Name!", "名字不合法", ""))

	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	list, err := h.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 6 { // 3 内置 + 3 坏文件
		t.Fatalf("清单 = %d 条，坏文件被静默丢掉了: %+v", len(list), list)
	}
	for name, frag := range map[string]string{
		"no-frontmatter": "frontmatter",
		"unclosed":       "frontmatter",
		"bad-name":       "不合法",
	} {
		s, ok := findSkill(list, name)
		if !ok {
			t.Errorf("%s 不在清单里", name)
			continue
		}
		if s.Status != "unavailable" || !strings.Contains(s.Unavailable, frag) {
			t.Errorf("%s = %+v，期望 unavailable 且原因含 %q", name, s, frag)
		}
	}
}

// ── 6. Get ───────────────────────────────────────────────────

func TestGetBodyAndNotFound(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	s, err := h.Get(context.Background(), "astra-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Body, "# 何时使用") || !strings.Contains(s.Body, "name: astra-run") {
		t.Errorf("Get 没返回 SKILL.md 全文: %q", s.Body)
	}
	if s.Status != "ready" {
		t.Errorf("status = %q", s.Status)
	}
	if _, err := h.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get 不存在的 err = %v，期望 ErrNotFound", err)
	}
	// 新文件直接落盘后 Get 也能立刻看到。
	writeSkill(t, dir, "fresh", doc("fresh", "刚落的", ""))
	if _, err := h.Get(context.Background(), "fresh"); err != nil {
		t.Fatalf("Get 没做懒刷新: %v", err)
	}
}

// ── 7. Publish ───────────────────────────────────────────────

func TestPublishWritesRefreshesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	ctx := context.Background()

	path, err := h.Publish(ctx, "my-skill", "# 纯正文\n", []string{"astra-cli"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := filepath.Join(dir, "my-skill", "SKILL.md"); path != want {
		t.Fatalf("路径 = %q，期望 %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("SKILL.md 权限/存在性不对: %v %v", info, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "requires: [astra-cli]") {
		t.Errorf("Publish 没把 requires 写进 frontmatter:\n%s", b)
	}
	list, _ := h.List(ctx)
	s, ok := findSkill(list, "my-skill")
	if !ok || s.Status != "ready" || s.Source != "distilled" {
		t.Fatalf("Publish 后索引 = %+v (ok=%v)", s, ok)
	}

	// 同名覆盖前留 .bak.<unix>。
	if _, err := h.Publish(ctx, "my-skill", "# 第二版\n", nil); err != nil {
		t.Fatalf("覆盖 Publish: %v", err)
	}
	baks, _ := filepath.Glob(filepath.Join(dir, "my-skill", "SKILL.md.bak.*"))
	if len(baks) != 1 {
		t.Fatalf("备份文件 = %v，期望 1 个", baks)
	}
	old, _ := os.ReadFile(baks[0])
	if !strings.Contains(string(old), "requires: [astra-cli]") {
		t.Errorf("备份内容不是上一版: %q", old)
	}

	for _, bad := range []string{"../evil", "Bad Name", "", "a/b"} {
		if _, err := h.Publish(ctx, bad, "x", nil); err == nil {
			t.Errorf("Publish(%q) 应当被拒", bad)
		}
	}
}

// ── 8. 并发 ──────────────────────────────────────────────────

func TestConcurrentListPublishKeepsIndexValid(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	ctx := context.Background()

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.List(ctx); err != nil {
				errs <- err
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.Publish(ctx, fmt.Sprintf("skill-%d", i), doc(fmt.Sprintf("skill-%d", i), "并发写入", ""), nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发操作报错: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatal(err)
	}
	var idx indexFile
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("并发把 index.json 写坏了: %v\n%s", err, raw)
	}
	if len(idx.Skills) != 3+n {
		t.Errorf("索引条目 = %d，期望 %d", len(idx.Skills), 3+n)
	}
}

// ── 9. 事件广播 ──────────────────────────────────────────────

type eventCollector struct {
	mu  sync.Mutex
	evs []store.Event
}

func (c *eventCollector) fn(ev store.Event) {
	if ev.Type != eventSkillPublished {
		return
	}
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
}

func (c *eventCollector) snapshot() []store.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.Event(nil), c.evs...)
}

func (c *eventCollector) names() []string {
	var out []string
	for _, ev := range c.snapshot() {
		var p struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			out = append(out, p.Name+"/"+p.Source)
		}
	}
	return out
}

func TestEventBroadcastOnNewSkill(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	c := &eventCollector{}
	bus.OnGlobal(c.fn)

	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, bus)
	if got := c.names(); len(got) != 0 {
		t.Fatalf("Open 不应广播启动噪声: %v", got)
	}
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.names(); len(got) != 0 {
		t.Fatalf("无变更的 List 不应广播: %v", got)
	}

	// 模拟 distill.Accept：只写文件，List 自动发现并广播。
	writeSkill(t, dir, "auto-found", doc("auto-found", "自动发现", ""))
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.names()
	if len(got) != 1 || got[0] != "auto-found/distilled" {
		t.Fatalf("广播 = %v，期望 auto-found/distilled", got)
	}

	// 重复 List 不重复广播。
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.names(); len(got) != 1 {
		t.Fatalf("重复 List 重复广播: %v", got)
	}

	// Publish 显式发布，覆盖同名也要广播。
	if _, err := h.Publish(context.Background(), "pub", doc("pub", "发布", ""), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.names(); len(got) != 2 || got[1] != "pub/distilled" {
		t.Fatalf("Publish 广播 = %v", got)
	}
	// RunID 可空。
	for _, ev := range c.snapshot() {
		if ev.RunID != "" {
			t.Errorf("skill_published 的 RunID 应为空，实际 %q", ev.RunID)
		}
	}
}

func TestRefreshRebuildsAndBroadcasts(t *testing.T) {
	dir := t.TempDir()
	bus := eventbus.New()
	c := &eventCollector{}
	bus.OnGlobal(c.fn)
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, bus)

	writeSkill(t, dir, "by-refresh", doc("by-refresh", "显式刷新", ""))
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.names(); len(got) != 1 || got[0] != "by-refresh/distilled" {
		t.Fatalf("Refresh 广播 = %v", got)
	}
	list, err := h.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findSkill(list, "by-refresh"); !ok {
		t.Fatal("Refresh 后清单里没有新 skill")
	}
}

func TestCtxCancellationIsRespected(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, Config{SkillDir: dir, CLIBin: execBin(t)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.List(ctx); err == nil {
		t.Error("List 忽略了已取消的 ctx")
	}
	if _, err := h.Get(ctx, "astra-run"); err == nil {
		t.Error("Get 忽略了已取消的 ctx")
	}
}

func TestDefaultSkillDirMatchesDistill(t *testing.T) {
	// 不设 SkillDir 时落到 ~/.astra/skills（与 distill.Open 的默认一致），
	// 不依赖 cwd，也不需要仓库 skills/ 存在。
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := openHub(t, Config{CLIBin: execBin(t)}, nil)
	if _, err := h.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".astra", "skills", "astra-run", "SKILL.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("内置 skill 没落到默认目录 %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".astra", "skills", indexFileName)); err != nil {
		t.Fatalf("默认目录下没有 index.json: %v", err)
	}
}
