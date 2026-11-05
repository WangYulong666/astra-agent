package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidateID 是安全边界，不是表单校验。
// ID 直接拼进文件路径，放过一个 ".." 就等于允许 RemoveAll 删到宿主任意位置。
func TestValidateID(t *testing.T) {
	good := []string{"dev", "abc", "s-1", "a.b_c", "A9", strings.Repeat("x", 64)}
	for _, id := range good {
		if err := validateID(id); err != nil {
			t.Errorf("合法 ID %q 被拒: %v", id, err)
		}
	}

	bad := []string{
		"", "..", ".", "../etc", "a/b", `a\b`, "-lead", ".hidden",
		"a..b", strings.Repeat("x", 65), "a b", "a;b", "a$b",
	}
	for _, id := range bad {
		if err := validateID(id); err == nil {
			t.Errorf("非法 ID %q 被放行——这是路径穿越风险", id)
		}
	}
}

func TestProfileCoversWorkspaceOnly(t *testing.T) {
	l := NewLocal(Options{Root: "/tmp/astra-test-root"})
	m := &meta{ID: "dev", Workspace: "/tmp/astra-test-root/dev/workspace", Home: "/tmp/astra-test-root/dev/home"}
	p := l.profile(m)

	if !strings.HasPrefix(p, "(version 1)") {
		t.Errorf("profile 必须以 (version 1) 开头，实际: %q", p[:min(30, len(p))])
	}
	// 关键：默认拒绝写，再显式放行 workspace / home
	if !strings.Contains(p, "(deny file-write*)") {
		t.Error("profile 缺少默认的写拒绝规则")
	}
	for _, must := range []string{m.Workspace, m.Home, "/private/tmp"} {
		if !strings.Contains(p, must) {
			t.Errorf("profile 未放行 %q", must)
		}
	}
	// 括号必须配平，否则 sandbox-exec 直接语法错误（第一次就是这么踩的）
	if strings.Count(p, "(") != strings.Count(p, ")") {
		t.Errorf("profile 括号不配平: %s", p)
	}
}

func TestReqToken(t *testing.T) {
	if got := reqToken(CreateRequest{Token: "fixed"}); got != "fixed" {
		t.Errorf("期望沿用手传 token，得到 %q", got)
	}
	a := reqToken(CreateRequest{})
	b := reqToken(CreateRequest{})
	if a == "" || len(a) < 20 {
		t.Errorf("自动生成的 token 太短: %q", a)
	}
	if a == b {
		t.Error("两次生成的 token 相同，随机源有问题")
	}
}

func TestPortFree(t *testing.T) {
	p, err := pickFreePort(45000)
	if err != nil {
		t.Fatalf("找不到空闲端口: %v", err)
	}
	if !portFree(p) {
		t.Errorf("pickFreePort 返回的 %d 却不可用", p)
	}
	if portFree(0) {
		t.Error("端口 0 不应被判为可用")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── 集成测试 ─────────────────────────────────────────────────
//
// 真的起 acp-proxy + 一个假 agent（cat，读 stdin 会一直阻塞 → 进程存活 → health 200），
// 这样才能验证端口保持、目录布局、Destroy 真删这些「单元测不出来」的行为。

func buildProxy(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "acp-proxy")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/acp-proxy")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("构建 acp-proxy 失败，跳过集成测试: %v\n%s", err, b)
	}
	return out
}

func TestLocalLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("集成测试，-short 下跳过")
	}
	proxy := buildProxy(t)
	root := t.TempDir()
	l := NewLocal(Options{Root: root, ProxyBin: proxy})
	ctx := context.Background()

	sb, err := l.Create(ctx, CreateRequest{
		ID:    "it-1",
		Agent: AgentSpec{Command: []string{"cat"}}, // 假 agent：活着但不讲 ACP
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 目录布局
	for _, sub := range []string{"workspace", "home", "logs", "snapshots"} {
		if _, err := os.Stat(filepath.Join(root, "it-1", sub)); err != nil {
			t.Errorf("缺少目录 %s: %v", sub, err)
		}
	}
	// meta 必须 0600——里面有 Bearer token
	fi, err := os.Stat(filepath.Join(root, "it-1", "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("meta.json 权限应为 0600（含 token），实际 %o", perm)
	}

	// 幂等：同 ID 再建应接管，端口不变
	sb2, err := l.Create(ctx, CreateRequest{ID: "it-1", Agent: AgentSpec{Command: []string{"cat"}}})
	if err != nil {
		t.Fatalf("重复 Create: %v", err)
	}
	if sb2.Endpoint().URL != sb.Endpoint().URL {
		t.Errorf("幂等创建换了端点: %s → %s", sb.Endpoint().URL, sb2.Endpoint().URL)
	}

	// List
	infos, err := l.List(ctx)
	if err != nil || len(infos) != 1 {
		t.Fatalf("List: %v, %d 项", err, len(infos))
	}
	if infos[0].State != StateRunning {
		t.Errorf("状态应为 running，实际 %s", infos[0].State)
	}

	// Exec
	res, err := sb.Exec(ctx, []string{"sh", "-c", "echo hi"}, ExecOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "hi" {
		t.Errorf("Exec 输出异常: %q", res.Stdout)
	}

	// Snapshot
	snapPath, err := sb.Snapshot(ctx, "base")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Errorf("快照目录不存在: %v", err)
	}

	// Pause 后端点必须保持不变（生产语义：pause/resume 不改数据面地址）
	urlBefore := sb.Endpoint().URL
	if err := sb.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := l.Acquire(ctx, "it-1"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("暂停后 Acquire 应返回 ErrNotRunning，实际 %v", err)
	}

	// 关键回归：重启时**故意不传** Agent.Command。
	// 实现必须从 meta 里把上次的 agent 命令读回来；如果 meta 存的是「请求里的原始值」
	// （调用方没传时就是空），spawn 会不加 -agent 而让 acp-proxy 退回内置默认，
	// 表现为「首次能起、重启必挂」——这个坑在 make dev-cycle 里真实踩过。
	if _, err := l.Create(ctx, CreateRequest{ID: "it-1"}); err != nil {
		t.Fatalf("暂停后重启（不传 agent 命令）: %v", err)
	}
	if got, _ := l.Acquire(ctx, "it-1"); got == nil || got.Endpoint().URL != urlBefore {
		t.Errorf("重启后端点变了: %s → %v", urlBefore, got)
	}

	// meta 里记的必须是解析后的完整命令
	var mm meta
	raw, err := os.ReadFile(filepath.Join(root, "it-1", "meta.json"))
	if err != nil {
		t.Fatalf("读 meta: %v", err)
	}
	if err := json.Unmarshal(raw, &mm); err != nil {
		t.Fatalf("解析 meta: %v", err)
	}
	if len(mm.Agent) == 0 || mm.Agent[0] != "cat" {
		t.Errorf("meta.Agent 应为 [cat]，实际 %v —— 重启会因此丢掉 agent 命令", mm.Agent)
	}

	// Destroy 必须真删目录（曾经的 bug：对已停沙箱打印成功但什么都没删）
	if err := l.Destroy(ctx, "it-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "it-1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Destroy 后目录仍存在——这是假成功")
	}
	if err := l.Destroy(ctx, "it-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复 Destroy 应返回 ErrNotFound，实际 %v", err)
	}
}

func TestNewRejectsUnknownProvider(t *testing.T) {
	if _, err := New("docker", Options{}); err == nil {
		t.Error("未知 provider 应该报错而不是静默回退")
	}
	for _, name := range []string{"local", "applecontainer", "cube"} {
		p, err := New(name, Options{})
		if err != nil || p.Name() == "" {
			t.Errorf("provider %q 构造失败: %v", name, err)
		}
	}
}

func TestDSHTrimPathIsContentAddressed(t *testing.T) {
	p1, ok := DSHTrimPath()
	if !ok {
		t.Fatal("取不到裁剪层落盘路径")
	}
	b, err := os.ReadFile(p1)
	if err != nil {
		t.Fatalf("读回裁剪层: %v", err)
	}
	if !strings.Contains(string(b), "llm-pi-ai") {
		t.Error("裁剪层内容不对：没有禁用 llm-pi-ai，dsh 启动时会把 import 警告打到 stdout 上")
	}
	// 二次调用必须命中同一个文件：否则每次重启都堆一个临时文件。
	p2, _ := DSHTrimPath()
	if p1 != p2 {
		t.Fatalf("两次落盘路径不同：%s vs %s", p1, p2)
	}
}

// 沙箱不能嵌套：agent 声明自带沙箱时，local provider 必须不再包 sandbox-exec。
// 否则内层 sandbox_apply 会以 Operation not permitted 失败（macOS 硬限制）。
func TestSelfSandboxDisablesOuterSeatbelt(t *testing.T) {
	l := NewLocal(Options{Root: t.TempDir()})
	if !l.strictAvailable() {
		t.Skip("本机没有 sandbox-exec，跳过")
	}
	if !l.strictFor(AgentSpec{}) {
		t.Error("默认 agent 应当由平台包裹 sandbox-exec")
	}
	if l.strictFor(AgentSpec{SelfSandbox: true}) {
		t.Error("agent 自带沙箱时不应再包 sandbox-exec（会触发 Operation not permitted）")
	}
}
