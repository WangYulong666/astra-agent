package distill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 测试全部走真实 store.OpenFile（tempdir）+ 真实事件流，不打桩：
// 还原逻辑的价值就在「事件流长什么样」上，mock 掉它等于什么都没测。

type harness struct {
	t   *testing.T
	st  *store.File
	d   Distiller
	dir string
}

func newHarness(t *testing.T, maxSteps int) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenFile(filepath.Join(dir, "data"), store.Options{Sync: false})
	if err != nil {
		t.Fatalf("store.OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d, err := Open(Config{Enabled: true, SkillDir: filepath.Join(dir, "skills"), MaxSteps: maxSteps}, st)
	if err != nil {
		t.Fatalf("distill.Open: %v", err)
	}
	if !d.Enabled() {
		t.Fatal("Enabled() = false")
	}
	h := &harness{t: t, st: st, d: d, dir: dir}
	if err := st.CreateSession(context.Background(), store.Session{
		ID: "sess-1", TenantID: "t1", UserID: "u1", SandboxID: "sess-1",
		Status: store.SessionActive,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return h
}

// newRun 造一个 run 并灌入事件流。
func (h *harness) newRun(input, status string, events ...store.NewEvent) store.Run {
	h.t.Helper()
	ctx := context.Background()
	id := "run-" + NewID()[:8]
	if err := h.st.CreateRun(ctx, store.Run{ID: id, SessionID: "sess-1", Status: store.RunRunning, Input: input}); err != nil {
		h.t.Fatalf("CreateRun: %v", err)
	}
	for _, e := range events {
		if _, err := h.st.Append(ctx, id, e); err != nil {
			h.t.Fatalf("Append: %v", err)
		}
	}
	run, err := h.st.GetRun(ctx, id)
	if err != nil {
		h.t.Fatalf("GetRun: %v", err)
	}
	run.Status = status
	if err := h.st.UpdateRun(ctx, run); err != nil {
		h.t.Fatalf("UpdateRun: %v", err)
	}
	return run
}

func toolStart(id, title, kind string, input map[string]any) store.NewEvent {
	return store.NewEvent{Type: store.EventToolStart, Payload: map[string]any{
		"toolCallId": id, "title": title, "kind": kind, "status": "pending", "rawInput": input,
	}}
}

func toolEnd(id, status string) store.NewEvent {
	return store.NewEvent{Type: store.EventToolUpdate, Payload: map[string]any{
		"toolCallId": id, "status": status,
	}}
}

func approvalResult(id, decision string) store.NewEvent {
	return store.NewEvent{Type: store.EventApprovalResult, Payload: map[string]any{
		"toolCallId": id, "decision": decision,
	}}
}

func (h *harness) distill(runID string) (Draft, error) {
	h.t.Helper()
	return h.d.Distill(context.Background(), Request{
		TenantID: "t1", UserID: "u1", RunID: runID, SessionID: "sess-1",
	})
}

// ── 1. 事件 → 步骤还原 ───────────────────────────────────────

func TestDistillReplaysSteps(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("go build the project then write dist/note.md",
		store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "dist/note.md", "content": "hi"}),
		toolEnd("t2", "failed"),
		toolStart("t3", "read", "read", map[string]any{"file_path": "README.md"}),
		toolEnd("t3", "pending"),
		approvalResult("t3", "deny"),
		toolStart("t4", "bash", "execute", map[string]any{"command": "mkdir -p dist"}),
		toolEnd("t4", "completed"),
	)

	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(d.Steps) != 4 {
		t.Fatalf("步骤数 = %d，期望 4：%+v", len(d.Steps), d.Steps)
	}
	want := []struct {
		tool, cmd, outcome string
	}{
		{"bash", "go build ./...", OutcomeOK},
		{"write", "dist/note.md", OutcomeFailed},
		{"read", "README.md", OutcomeDenied},
		{"bash", "mkdir -p dist", OutcomeOK},
	}
	for i, w := range want {
		got := d.Steps[i]
		if got.Index != i+1 || got.Tool != w.tool || got.Command != w.cmd || got.Outcome != w.outcome {
			t.Errorf("第 %d 步 = %+v，期望 %+v", i+1, got, w)
		}
		if got.Summary == "" {
			t.Errorf("第 %d 步没有 Summary", i+1)
		}
	}
	if d.Name != "go-build-project" {
		t.Errorf("name = %q，期望 go-build-project", d.Name)
	}
	if d.Status != StatusDraft || d.RunID != run.ID || d.TenantID != "t1" {
		t.Errorf("草稿元信息不对: %+v", d)
	}
	for _, frag := range []string{"# 何时使用", "# 步骤", "# 注意", "1. `bash`", "2. `write`", "被审批拒绝"} {
		if !strings.Contains(d.Body, frag) {
			t.Errorf("正文缺少 %q\n%s", frag, d.Body)
		}
	}
}

// 其他工具没有已知关键字段时，退化成入参 JSON 前 120 字。
func TestDistillFallsBackToRawInput(t *testing.T) {
	h := newHarness(t, 40)
	long := strings.Repeat("x", 200)
	run := h.newRun("fetch the docs", store.RunCompleted,
		toolStart("t1", "fetch", "fetch", map[string]any{"headers": map[string]any{"X": long}}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "ls"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if got := d.Steps[0].Tool; got != "fetch" {
		t.Errorf("tool = %q，期望 fetch", got)
	}
	if n := len([]rune(d.Steps[0].Command)); n > 121 {
		t.Errorf("兜底摘要没截断：%d 字", n)
	}
	if !strings.Contains(d.Steps[0].Command, "headers") {
		t.Errorf("兜底摘要不是入参 JSON: %q", d.Steps[0].Command)
	}
}

// ── 2. 校验 ─────────────────────────────────────────────────

func TestDistillRejectsNonTerminalRun(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("ls", store.RunRunning,
		toolStart("t1", "bash", "execute", map[string]any{"command": "ls"}),
		toolEnd("t1", "completed"),
	)
	if _, err := h.distill(run.ID); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("err = %v，期望 ErrNotTerminal", err)
	}
}

func TestDistillRejectsTooFewSteps(t *testing.T) {
	h := newHarness(t, 40)

	// 0 步：纯问答。
	qa := h.newRun("1+1 等于几", store.RunCompleted)
	if _, err := h.distill(qa.ID); !errors.Is(err, ErrTooFewSteps) {
		t.Fatalf("纯问答 err = %v，期望 ErrTooFewSteps", err)
	}

	// 1 步：一句话命令没有复用价值。
	one := h.newRun("看下目录", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "ls"}),
		toolEnd("t1", "completed"),
	)
	if _, err := h.distill(one.ID); !errors.Is(err, ErrTooFewSteps) {
		t.Fatalf("单步 err = %v，期望 ErrTooFewSteps", err)
	}

	// 2 步但只有 1 步成功：不计入。
	mixed := h.newRun("构建并写文件", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "a.md"}),
		toolEnd("t2", "failed"),
	)
	if _, err := h.distill(mixed.ID); !errors.Is(err, ErrTooFewSteps) {
		t.Fatalf("一成一败 err = %v，期望 ErrTooFewSteps", err)
	}

	// 同一 toolCallId 的多条 update 不能算成多步。
	dup := h.newRun("跑一遍构建", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "in_progress"),
		toolEnd("t1", "completed"),
	)
	if _, err := h.distill(dup.ID); !errors.Is(err, ErrTooFewSteps) {
		t.Fatalf("重复 update 被当成多步: err = %v", err)
	}
}

func TestDistillTruncatesLongTrace(t *testing.T) {
	h := newHarness(t, 2)
	run := h.newRun("loop over five files", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "echo 1"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "echo 2"}),
		toolEnd("t2", "completed"),
		toolStart("t3", "bash", "execute", map[string]any{"command": "echo 3"}),
		toolEnd("t3", "completed"),
		toolStart("t4", "bash", "execute", map[string]any{"command": "echo 4"}),
		toolEnd("t4", "completed"),
		toolStart("t5", "bash", "execute", map[string]any{"command": "echo 5"}),
		toolEnd("t5", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(d.Steps) != 2 {
		t.Fatalf("步骤数 = %d，期望截断到 2", len(d.Steps))
	}
	if d.Steps[1].Command != "echo 2" {
		t.Errorf("截断保留了错误的步骤: %+v", d.Steps)
	}
	if !strings.Contains(d.Body, "另有 3 步") {
		t.Errorf("正文没注明省略步数:\n%s", d.Body)
	}
}

func TestDistillRejectsForeignTenant(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("go build ./...", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "a"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "b"}),
		toolEnd("t2", "completed"),
	)
	own, err := h.d.List(context.Background(), "t2")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(own) != 0 {
		t.Fatalf("t2 看到了 t1 的草稿: %+v", own)
	}
	_, err = h.d.Distill(context.Background(), Request{TenantID: "t2", UserID: "u2", RunID: run.ID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户 err = %v，期望 ErrNotFound", err)
	}
}

// ── 3. Accept 落盘 ──────────────────────────────────────────

func TestAcceptWritesSkillFile(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("go build and then write note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "note.md"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	path, err := h.d.Accept(context.Background(), "t1", d.ID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if want := filepath.Join(h.dir, "skills", d.Name, "SKILL.md"); path != want {
		t.Fatalf("落地路径 = %q，期望 %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读落地文件: %v", err)
	}
	if string(b) != d.Body {
		t.Fatal("落地内容与草稿正文不一致")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("文件权限 = %o，期望 644", info.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o755 {
		t.Errorf("目录权限 = %v，期望 755", di.Mode().Perm())
	}

	list, err := h.d.List(context.Background(), "t1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Status != StatusAccepted || list[0].Path != path {
		t.Fatalf("accept 后草稿状态不对: %+v", list)
	}

	// 草稿要真的落盘：重开一个实例仍能读到。
	if _, err := os.Stat(filepath.Join(h.dir, "drafts.json")); err != nil {
		t.Fatalf("drafts.json 不存在: %v", err)
	}
	if info, err := os.Stat(filepath.Join(h.dir, "drafts.json")); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("drafts.json 权限 = %o，期望 600", info.Mode().Perm())
	}

	// 换一个实例（模拟进程重启）仍能读到 accepted 状态与落地路径。
	d2, err := Open(Config{Enabled: true, SkillDir: filepath.Join(h.dir, "skills"), MaxSteps: 40}, h.st)
	if err != nil {
		t.Fatalf("重开 Open: %v", err)
	}
	re, err := d2.List(context.Background(), "t1")
	if err != nil || len(re) != 1 || re[0].Path != path || re[0].Status != StatusAccepted {
		t.Fatalf("重启后草稿 = %+v (err=%v)", re, err)
	}
}

func TestAcceptBacksUpExistingSkill(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("build and note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "make"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "out.md"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	dir := filepath.Join(h.dir, "skills", d.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(target, []byte("人手改过的旧版\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := h.d.Accept(context.Background(), "t1", d.ID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	matches, err := filepath.Glob(target + ".bak.*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("备份文件 = %v (err=%v)，期望 1 个 .bak.<unix>", matches, err)
	}
	old, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "人手改过的旧版\n" {
		t.Errorf("备份内容 = %q", old)
	}
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != d.Body {
		t.Error("覆盖后内容不是新草稿")
	}
}

// 路径穿越必须被拒：名字是唯一会拼进落盘路径的用户可控字段。
func TestAcceptRejectsPathTraversal(t *testing.T) {
	h := newHarness(t, 40)
	evil := []string{
		"../../etc/passwd",
		"..",
		"/etc/passwd",
		"a/../../b",
		`..\..\windows\system32`,
		"..%2f..%2fetc",
		"task.v2", // 点号一律拒
		"名前",      // 非 ASCII 一律拒
		"a b/c",   // 空格会被规范化成连字符，但斜杠仍然拒
	}
	for _, name := range evil {
		d := Draft{ID: NewID(), TenantID: "t1", Name: name, Body: "x", Status: StatusDraft}
		if err := h.d.(*distiller).saveDrafts([]Draft{d}); err != nil {
			t.Fatalf("saveDrafts(%q): %v", name, err)
		}
		path, err := h.d.Accept(context.Background(), "t1", d.ID)
		if err == nil {
			t.Fatalf("Accept(%q) 成功了，路径 %q —— 路径穿越没挡住", name, path)
		}
		if !errors.Is(err, ErrBadName) {
			t.Fatalf("Accept(%q) err = %v，期望 ErrBadName", name, err)
		}
	}

	// Distill 的 Name 参数走同一条校验。
	run := h.newRun("build and note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "make"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "make install"}),
		toolEnd("t2", "completed"),
	)
	_, err := h.d.Distill(context.Background(), Request{
		TenantID: "t1", UserID: "u1", RunID: run.ID, Name: "../../etc/passwd",
	})
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("Distill 非法 Name err = %v，期望 ErrBadName", err)
	}

	// 确认什么都没有被写到 skills 目录之外。
	if _, err := os.Stat(filepath.Join(h.dir, "etc")); err == nil {
		t.Fatal("skills 目录外出现了文件，路径穿越生效了")
	}
}

// ── 4. frontmatter ──────────────────────────────────────────

func TestFrontmatterIsParseable(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun(`把 dist 打出来: go build -o dist/app . 然后 write 一个 release.md`,
		store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build -o dist/app ."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "release.md"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	fm, err := parseFrontmatter(d.Body)
	if err != nil {
		t.Fatalf("frontmatter 解析失败: %v\n%s", err, d.Body)
	}
	if fm["name"] != d.Name {
		t.Errorf("name = %q，期望 %q", fm["name"], d.Name)
	}
	if !slugValidRe.MatchString(fm["name"]) {
		t.Errorf("name %q 不是合法 slug", fm["name"])
	}
	if fm["version"] != "1" {
		t.Errorf("version = %q", fm["version"])
	}
	if fm["requires"] != "[astra-cli]" {
		t.Errorf("requires = %q", fm["requires"])
	}
	if !strings.HasPrefix(fm["description"], `"`) || !strings.HasSuffix(fm["description"], `"`) {
		t.Errorf("description 不是带引号的标量: %q", fm["description"])
	}
	for _, h2 := range []string{"# 何时使用", "# 步骤", "# 注意"} {
		if !strings.Contains(d.Body, h2) {
			t.Errorf("缺段落 %q", h2)
		}
	}
	// 指令里的冒号不能把 description 变成 map。
	if strings.Count(strings.SplitN(d.Body, "---\n", 3)[1], ":") < 4 {
		t.Errorf("frontmatter 字段不全:\n%s", d.Body)
	}
}

// parseFrontmatter 是一个足够严格的迷你 YAML 解析器：只认 `key: scalar` 一行一条。
// 用它而不是引 yaml 依赖，同时它比 strings.Contains 更能证明格式真的对。
func parseFrontmatter(body string) (map[string]string, error) {
	if !strings.HasPrefix(body, "---\n") {
		return nil, errors.New("正文不以 ---\\n 开头")
	}
	rest := body[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, errors.New("frontmatter 没有闭合")
	}
	out := map[string]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			return nil, fmt.Errorf("无法解析的行: %q", line)
		}
		if k != strings.TrimSpace(k) {
			return nil, fmt.Errorf("key 带空白: %q", k)
		}
		out[k] = v
	}
	return out, nil
}

// ── 5. 并发 Accept ──────────────────────────────────────────

func TestConcurrentAcceptDoesNotCorruptFile(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("go build then write note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "go build ./..."}),
		toolEnd("t1", "completed"),
		toolStart("t2", "write", "edit", map[string]any{"file_path": "note.md"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = h.d.Accept(context.Background(), "t1", d.ID)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发 Accept[%d]: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatalf("并发 Accept 落到不同路径: %v", paths)
		}
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != d.Body {
		t.Fatal("并发写入把文件写坏了")
	}
	if _, err := parseFrontmatter(string(b)); err != nil {
		t.Fatalf("落地文件 frontmatter 坏了: %v", err)
	}
	// 临时文件不能留在目录里（否则 skills/ 下会堆垃圾）。
	left, err := filepath.Glob(filepath.Join(filepath.Dir(paths[0]), ".tmp-*"))
	if err != nil || len(left) != 0 {
		t.Errorf("残留临时文件: %v", left)
	}
	// 草稿列表也得是完好的 JSON（并发写的另一半）。
	if _, err := h.d.List(context.Background(), "t1"); err != nil {
		t.Fatalf("并发后草稿文件损坏: %v", err)
	}
}

// ── 6. 其它 ────────────────────────────────────────────────

func TestRejectAndNotFound(t *testing.T) {
	h := newHarness(t, 40)
	run := h.newRun("build and note", store.RunCompleted,
		toolStart("t1", "bash", "execute", map[string]any{"command": "make"}),
		toolEnd("t1", "completed"),
		toolStart("t2", "bash", "execute", map[string]any{"command": "make check"}),
		toolEnd("t2", "completed"),
	)
	d, err := h.distill(run.ID)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if err := h.d.Reject(context.Background(), "t1", d.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	list, err := h.d.List(context.Background(), "t1")
	if err != nil || len(list) != 1 || list[0].Status != StatusRejected {
		t.Fatalf("reject 后状态 = %+v (err=%v)", list, err)
	}
	if _, err := h.d.Accept(context.Background(), "t1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在草稿 err = %v，期望 ErrNotFound", err)
	}
	if err := h.d.Reject(context.Background(), "t1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reject 不存在 err = %v，期望 ErrNotFound", err)
	}
	// 别的租户看不到、也 accept 不了。
	if _, err := h.d.Accept(context.Background(), "t2", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户 accept err = %v，期望 ErrNotFound", err)
	}
}

func TestOpenDisabled(t *testing.T) {
	d, err := Open(Config{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("Open(disabled): %v", err)
	}
	if d.Enabled() {
		t.Fatal("Enabled() 应为 false")
	}
	if _, err := d.List(context.Background(), "t1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v，期望 ErrDisabled", err)
	}
	if _, err := Open(Config{Enabled: true, SkillDir: t.TempDir()}, nil); err == nil {
		t.Fatal("缺 store 应当报错")
	}
}

func TestSlugFallback(t *testing.T) {
	if got := SlugFrom("请把构建产物打包", "run-abc123def"); got != "task-run-ab" {
		t.Errorf("中文指令 slug = %q，期望 task-run-ab", got)
	}
	if got := SlugFrom("go build the project", "run-x"); got != "go-build-project" {
		t.Errorf("英文指令 slug = %q", got)
	}
	if got := SlugFrom("", "abcdef123456"); got != "task-abcdef" {
		t.Errorf("空指令 slug = %q，期望 task-abcdef", got)
	}
}
