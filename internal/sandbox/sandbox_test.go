package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// ── 出网强制边界（NetworkPolicy / Seatbelt）───────────────────────────

// legacyProfileString 复现「未启用本能力时」的 profile 算法，用来做逐字节兼容断言。
func legacyProfileString(l *Local, m *meta) string {
	allow := []string{
		m.Workspace,
		m.Home,
		filepath.Join(l.dir(m.ID), "tmp"),
		filepath.Join(l.dir(m.ID), "logs"),
		filepath.Join(l.dir(m.ID), "snapshots"),
		"/private/tmp",
		"/private/var/folders",
		"/dev",
	}
	var b strings.Builder
	b.WriteString("(version 1)(allow default)(deny file-write*)(allow file-write*")
	for _, p := range allow {
		b.WriteString(fmt.Sprintf(" (subpath %q)", p))
	}
	b.WriteString(")\n")
	return b.String()
}

func TestProfileNetworkPolicyDisabledByteIdentical(t *testing.T) {
	l := NewLocal(Options{Root: "/tmp/astra-test-root"})
	m := &meta{ID: "dev", Workspace: "/tmp/astra-test-root/dev/workspace", Home: "/tmp/astra-test-root/dev/home"}
	want := legacyProfileString(l, m)

	cases := []struct {
		name string
		np   *NetworkPolicy
	}{
		{"nil", nil},
		{"disabled", &NetworkPolicy{Enabled: false}},
		{"disabled-with-junk", &NetworkPolicy{Enabled: false, ProxyAddr: "127.0.0.1:17890", ExtraAllow: []string{"127.0.0.1:7788"}}},
	}
	for _, tc := range cases {
		m.NetworkPolicy = tc.np
		if got := l.profile(m); got != want {
			t.Errorf("%s: Enabled=false 时 profile 必须与旧版逐字节相同\n got=%q\nwant=%q", tc.name, got, want)
		}
	}
}

func TestProfileNetworkPolicyEnabled(t *testing.T) {
	l := NewLocal(Options{Root: "/tmp/astra-test-root"})
	m := &meta{
		ID: "dev", Workspace: "/tmp/astra-test-root/dev/workspace", Home: "/tmp/astra-test-root/dev/home",
		NetworkPolicy: &NetworkPolicy{
			Enabled:    true,
			ProxyAddr:  "127.0.0.1:17890",
			ExtraAllow: []string{"127.0.0.1:7788", "localhost:9911"},
		},
	}
	p := l.profile(m)
	t.Logf("Enabled=true 完整 profile:\n%s", p)

	if !strings.HasPrefix(p, legacyProfileString(l, m)) {
		t.Error("网络规则必须追加在写限制收口之后，且 base 部分逐字节不变")
	}
	for _, must := range []string{
		"(deny network*)",
		`"localhost:17890"`,
		`"localhost:7788"`,
		`"localhost:9911"`,
		"(remote unix-socket)",
		`(allow network-inbound (local ip "localhost:*"))`,
		`(allow network-bind (local ip "localhost:*"))`,
	} {
		if !strings.Contains(p, must) {
			t.Errorf("profile 缺少 %q", must)
		}
	}
	// last-match-wins：deny 必须在任何具体 allow 之前。
	di := strings.Index(p, "(deny network*)")
	ai := strings.Index(p, `(allow network-outbound (remote ip "localhost:17890"))`)
	if di < 0 || ai < 0 || di >= ai {
		t.Error("(deny network*) 必须排在具体 allow 之前（SBPL last-match-wins）")
	}
	if strings.Count(p, "(") != strings.Count(p, ")") {
		t.Errorf("profile 括号不配平: %s", p)
	}
	// 数字 IP 作 network address host 在 macOS 26 编译失败，绝不能出现。
	if strings.Contains(p, `remote ip "127.0.0.1`) {
		t.Error("不能用数字 IP 作 remote ip 的 host（真机实测编译失败）")
	}
}

func TestProfileNetworkPolicyRejectsInjection(t *testing.T) {
	l := NewLocal(Options{Root: "/tmp/astra-test-root"})
	cases := []struct {
		name    string
		proxy   string
		extra   []string
		illegal []string // 明确列出非法、绝不该出现在 profile 里的值
		legalIn []string // 合法、应当出现在 profile 里的值（验证没被误伤）
	}{
		{"proxy-带分号注入", `127.0.0.1:17890")(allow network*)`, nil,
			[]string{`127.0.0.1:17890")(allow network*)`}, nil},
		{"proxy-带空格", "127.0.0.1:17890 ; (allow network*)", nil,
			[]string{"127.0.0.1:17890 ; (allow network*)"}, nil},
		{"proxy-无端口", "127.0.0.1", nil, []string{"127.0.0.1"}, nil},
		{"proxy-域名", "evil.example.com:8080", nil, []string{"evil.example.com:8080"}, nil},
		{"proxy-引号", `127.0.0.1:17"890`, nil, []string{`127.0.0.1:17"890`}, nil},
		{"proxy-端口越界", "127.0.0.1:99999", nil, []string{"127.0.0.1:99999"}, nil},
		{"extra-带括号注入", "127.0.0.1:17890", []string{`127.0.0.1:7788")(allow network*)`},
			[]string{`127.0.0.1:7788")(allow network*)`}, []string{`"localhost:17890"`}},
		{"extra-带分号", "127.0.0.1:17890", []string{"127.0.0.1:7788; deny"},
			[]string{"127.0.0.1:7788; deny"}, []string{`"localhost:17890"`}},
		{"extra-带引号", "127.0.0.1:17890", []string{`127.0.0.1:77"88`},
			[]string{`127.0.0.1:77"88`}, []string{`"localhost:17890"`}},
		{"extra-合法+非法混合", "127.0.0.1:17890", []string{"127.0.0.1:7788; (allow network*)", "localhost:9911"},
			[]string{"127.0.0.1:7788; (allow network*)"}, []string{`"localhost:9911"`}},
	}
	for _, tc := range cases {
		m := &meta{
			ID: "dev", Workspace: "/tmp/astra-test-root/dev/workspace", Home: "/tmp/astra-test-root/dev/home",
			NetworkPolicy: &NetworkPolicy{Enabled: true, ProxyAddr: tc.proxy, ExtraAllow: tc.extra},
		}
		p := l.profile(m)
		for _, bad := range tc.illegal {
			if strings.Contains(p, bad) {
				t.Errorf("%s: 非法值 %q 原样出现在 profile 里（SBPL 注入风险）", tc.name, bad)
			}
		}
		for _, ok := range tc.legalIn {
			if !strings.Contains(p, ok) {
				t.Errorf("%s: 合法项 %q 被误伤（应出现在 profile 里）", tc.name, ok)
			}
		}
		// 注入若成功，通常会拼出一个通配 allow；断言它不存在。
		if strings.Contains(p, "(allow network*)") {
			t.Errorf("%s: 出现通配 (allow network*)（注入成功）", tc.name)
		}
		// 非法输入不能让整层边界消失：deny 必须仍在（fail-closed）。
		if !strings.Contains(p, "(deny network*)") {
			t.Errorf("%s: 非法输入不应导致 (deny network*) 消失", tc.name)
		}
		if strings.Count(p, "(") != strings.Count(p, ")") {
			t.Errorf("%s: 括号不配平", tc.name)
		}
	}
}

func TestSbplAddr(t *testing.T) {
	good := map[string]string{
		"127.0.0.1:17890": "17890",
		"localhost:80":    "80",
		"[::1]:443":       "443",
		"127.0.0.1:*":     "*", // 允许 * 通配端口（调用方显式要求时）
		"localhost:*":     "*",
		"10.0.0.5:7788":   "7788", // 合法数值 IP 也通过校验（归一化后只放行回环端口）
	}
	for in, wantPort := range good {
		p, ok := sbplAddr(in)
		if !ok || p != wantPort {
			t.Errorf("sbplAddr(%q) = (%q,%v)，期望 (%q,true)", in, p, ok, wantPort)
		}
	}
	bad := []string{
		"", "127.0.0.1", "127.0.0.1:", ":17890",
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc", "127.0.0.1:-1",
		"127.0.0.1:17890 ", "127.0.0.1:17 890", "127.0.0.1:17890\t",
		`127.0.0.1:17"890`, "127.0.0.1:17890;", "127.0.0.1:17890)",
		"127.0.0.1:17890(", "127.0.0.1:17890\n(allow network*)",
		"evil.example.com:80", "*.openai.com:443", "127.0.0.1:17890:extra",
	}
	for _, in := range bad {
		if _, ok := sbplAddr(in); ok {
			t.Errorf("sbplAddr(%q) 应被拒绝（防注入 / 非 host:port）", in)
		}
	}
}

func TestMetaNetworkPolicyPersistence(t *testing.T) {
	root := t.TempDir()
	l := NewLocal(Options{Root: root})
	np := &NetworkPolicy{Enabled: true, ProxyAddr: "127.0.0.1:17890", ExtraAllow: []string{"127.0.0.1:7788"}}
	m := &meta{
		ID: "persist", Port: 41234, Token: "t",
		Workspace:     filepath.Join(root, "persist", "workspace"),
		Home:          filepath.Join(root, "persist", "home"),
		Agent:         []string{"cat"},
		State:         StateRunning,
		Strict:        true,
		NetworkPolicy: np,
	}
	if err := os.MkdirAll(l.dir(m.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.writeMeta(m); err != nil {
		t.Fatal(err)
	}

	back, err := l.readMeta(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.NetworkPolicy == nil {
		t.Fatal("meta 写盘再读回后 NetworkPolicy 丢失")
	}
	if !back.NetworkPolicy.Enabled || back.NetworkPolicy.ProxyAddr != np.ProxyAddr ||
		len(back.NetworkPolicy.ExtraAllow) != 1 || back.NetworkPolicy.ExtraAllow[0] != np.ExtraAllow[0] {
		t.Errorf("NetworkPolicy 读回不一致: %+v", back.NetworkPolicy)
	}

	// 模拟 Resume：Resume 用 spawn(s.m, AgentSpec{Command: s.m.Agent}) 重启，
	// 传入的 spec 里 NetworkPolicy 是 nil。spawn → spawnCommand 一律以 meta 为准，
	// 因此读回的 back 仍必须生成含出网边界的 profile。
	resumeSpec := AgentSpec{Command: back.Agent} // 注意：不带 NetworkPolicy
	_ = resumeSpec
	name, args := l.spawnCommand(back, "acp-proxy", []string{"-listen", "127.0.0.1:41234"})
	if name != "sandbox-exec" {
		t.Fatalf("Resume 后应仍套 sandbox-exec，实际 %q", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "(deny network*)") || !strings.Contains(joined, `"localhost:17890"`) {
		t.Errorf("Resume 后出网边界消失（spawnCommand 未以 meta 为准）: %q", joined)
	}
}

func TestNoNetworkRulesWhenStrictUnavailable(t *testing.T) {
	t.Setenv("ASTRA_NO_SEATBELT", "1")
	l := NewLocal(Options{Root: t.TempDir()})
	if l.strictAvailable() {
		t.Fatal("设置 ASTRA_NO_SEATBELT 后 strictAvailable() 应为 false")
	}
	spec := AgentSpec{NetworkPolicy: &NetworkPolicy{Enabled: true, ProxyAddr: "127.0.0.1:17890"}}
	strict := l.strictFor(spec) // strictAvailable() && !SelfSandbox
	if strict {
		t.Fatal("strict 不可用时 strictFor 应为 false")
	}
	m := &meta{ID: "x", Workspace: "/tmp/x/ws", Home: "/tmp/x/home", Strict: strict, NetworkPolicy: spec.NetworkPolicy}
	name, args := l.spawnCommand(m, "acp-proxy", []string{"-listen", "127.0.0.1:41000"})
	if name == "sandbox-exec" {
		t.Error("strict 不可用时不应套 sandbox-exec")
	}
	for _, a := range args {
		if a == "-p" || strings.Contains(a, "(deny network*)") {
			t.Errorf("strict 不可用时启动命令里不该出现 -p / 网络规则: %q", args)
		}
	}
	// profile() 作为纯函数仍会按 Enabled 生成规则，但 spawnCommand 在 m.Strict=false
	// 时根本不调用它——「出网强制边界」整体不生效，跟随 m.Strict，不单独绕过。
	t.Log("strict 不可用 → spawnCommand 不调用 profile → 网络规则不生效（跟随 m.Strict）")
}

// ── 真机验证 ─────────────────────────────────────────────────────────
//
// 两个用例分开，是因为「编译验证」与「apply 拦截验证」对环境的依赖不同：
// sandbox-exec 编译 profile 一定成功（除非语法写错），但 apply 一个含 deny 的
// profile 在「已被沙箱化的进程」里必然返回 sandbox_apply: Operation not permitted
// （macOS 沙箱不能嵌套，local.go 注释里也写了）。本会话的命令执行环境正是这种
// 嵌套环境，所以端到端拦截会 Skip；编译验证仍能真跑，用来锁住 SBPL 语法。

func envWithoutProxy(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "FTP_PROXY", "NO_PROXY":
			continue // 清掉代理，否则 curl 走 HTTP_PROXY 就测不到直连了
		}
		out = append(out, kv)
	}
	return out
}

// sbplCompileError 区分「SBPL 编译错误」与「编译通过但 apply 失败」。
func sbplCompileError(out string) bool {
	low := strings.ToLower(out)
	for _, kw := range []string{"must be", "invalid", "port missing", "backtrace", "expected", "syntax error", "unknown operation"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func seatbeltMeta(root string, np *NetworkPolicy) *meta {
	return &meta{
		ID: "e2e", Workspace: filepath.Join(root, "e2e", "workspace"),
		Home: filepath.Join(root, "e2e", "home"), NetworkPolicy: np,
	}
}

// TestSeatbeltProfileCompilesReal（真机）：把生成的完整 profile 交给
// /usr/bin/sandbox-exec 真实编译，断言没有语法错误。这能锁住两处真机实测的坑：
//   - (remote ip "127.0.0.1:17890") 这类数字 IP 写法编译失败（必须用 localhost:port）
//   - 各 form 的括号、operation、filter 组合合法
func TestSeatbeltProfileCompilesReal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("非 darwin，没有 Seatbelt，跳过真机编译验证")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("无 /usr/bin/sandbox-exec，跳过真机编译验证")
	}
	l := NewLocal(Options{Root: t.TempDir()})
	m := seatbeltMeta("/tmp/astra-compile", &NetworkPolicy{
		Enabled: true, ProxyAddr: "127.0.0.1:17890", ExtraAllow: []string{"127.0.0.1:7788"},
	})
	p := l.profile(m)
	t.Logf("真机编译的完整 SBPL profile:\n%s", p)

	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", p, "/usr/bin/true").CombinedOutput()
	combined := string(out)
	if sbplCompileError(combined) {
		t.Fatalf("sandbox-exec 编译 profile 失败（真机）：%v\n%s", err, combined)
	}
	switch {
	case strings.Contains(combined, "sandbox_apply"):
		t.Logf("真机：SBPL 编译通过；当前进程已被沙箱化，sandbox_apply 返回 Operation not permitted（嵌套，非语法问题）。输出：%s", strings.TrimSpace(combined))
	case err != nil:
		t.Fatalf("sandbox-exec 意外失败：%v\n%s", err, combined)
	default:
		t.Log("真机：SBPL 编译并 apply 成功")
	}
}

// TestSeatbeltNetworkLockE2E（真机）：端到端验证出网强制边界。
// 只有 sandbox-exec 能真正 apply 含 deny 的 profile 时才跑；嵌套沙箱环境 Skip 并说明。
func TestSeatbeltNetworkLockE2E(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("非 darwin，没有 Seatbelt，跳过真机端到端验证")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("无 /usr/bin/sandbox-exec，跳过真机端到端验证")
	}

	root := t.TempDir()
	l := NewLocal(Options{Root: root})
	locked := l.profile(seatbeltMeta(root, &NetworkPolicy{Enabled: true, ProxyAddr: "127.0.0.1:17890"}))
	unlocked := l.profile(seatbeltMeta(root, nil)) // Enabled=false 对照组

	// 探测能否 apply。必须区分「编译错误」和「apply EPERM」：
	// 只有 apply 成功才继续，绝不把 sandbox-exec 自己起不来当成「出网被挡」的假阳性。
	probeOut, probeErr := exec.Command("/usr/bin/sandbox-exec", "-p", locked, "/usr/bin/true").CombinedOutput()
	probe := string(probeOut)
	if sbplCompileError(probe) {
		t.Fatalf("SBPL 编译失败（真机）：%v\n%s", probeErr, probe)
	}
	if strings.Contains(probe, "sandbox_apply") {
		t.Skipf("当前 go test 进程已被沙箱化（macOS 沙箱不能嵌套）：sandbox-exec 对含 deny 的 profile 返回 sandbox_apply: Operation not permitted，端到端拦截无法在本会话实测；SBPL 语法已由 TestSeatbeltProfileCompilesReal 真机编译验证。探测输出：%s", strings.TrimSpace(probe))
	}
	if probeErr != nil {
		t.Fatalf("sandbox-exec 意外失败：%v\n%s", probeErr, probe)
	}

	const target = "https://www.baidu.com"
	runCurl := func(p string) (string, error) {
		cmd := exec.Command("/usr/bin/sandbox-exec", "-p", p, "/usr/bin/curl",
			"-sS", "--max-time", "3", "--noproxy", "*",
			"-o", "/dev/null", "-w", "HTTP=%{http_code}", target)
		cmd.Env = envWithoutProxy(os.Environ())
		b, err := cmd.CombinedOutput()
		return string(b), err
	}

	// 1) 出网直连必须被挡。
	blockedOut, blockedErr := runCurl(locked)
	t.Logf("[1] 锁定 profile 直连 %s: err=%v\n%s", target, blockedErr, blockedOut)
	if blockedErr == nil {
		t.Fatal("出网直连应被 SBPL 挡住，curl 却成功了——强制边界失效")
	}
	if strings.Contains(strings.ToLower(blockedOut), "could not resolve host") {
		t.Fatal("DNS 被误杀（unix-socket 放行没写对）：" + blockedOut)
	}
	if strings.Contains(blockedOut, "sandbox_apply") {
		t.Fatal("sandbox-exec 自身失败，不是规则拦截（假阳性）：" + blockedOut)
	}

	// 3) 对照组：Enabled=false 的旧 profile 必须能出去，证明不是网络本身不通。
	ctlOut, ctlErr := runCurl(unlocked)
	t.Logf("[3] 对照组 Enabled=false 直连 %s: err=%v\n%s", target, ctlErr, ctlOut)
	switch {
	case ctlErr == nil && strings.Contains(ctlOut, "HTTP=200"):
		t.Log("对照组成功联网：证明实验组是 SBPL 拦的，不是网络本身不通")
	case strings.Contains(ctlOut, "sandbox_apply") || strings.Contains(strings.ToLower(ctlOut), "operation not permitted"):
		t.Fatalf("对照组（Enabled=false）不该被网络规则拦却失败了: %s", ctlOut)
	default:
		t.Logf("注意：本机外网直连不可用（对照组 err=%v out=%q），按任务说明无法用对照组证明「非网络本身不通」；实验组结论以 DNS 正常 + 非零退出为准", ctlErr, ctlOut)
	}

	// 2) 本机代理端口必须能连：在沙箱外起 listener，沙箱内的命令连它。
	ln, err := net.Listen("tcp", "127.0.0.1:17890")
	if err != nil {
		t.Fatalf("起本机 127.0.0.1:17890 listener 失败: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	var connCmd *exec.Cmd
	if py, e := exec.LookPath("python3"); e == nil {
		connCmd = exec.Command("/usr/bin/sandbox-exec", "-p", locked, py, "-c",
			`import socket; s=socket.create_connection(("127.0.0.1",17890),3); s.close(); print("CONNECTED")`)
	} else if nc, e := exec.LookPath("nc"); e == nil {
		connCmd = exec.Command("/usr/bin/sandbox-exec", "-p", locked, nc, "-z", "-G", "3", "127.0.0.1", "17890")
	}
	if connCmd == nil {
		t.Log("无 python3 / nc，跳过本机代理端口连通断言")
	} else {
		connCmd.Env = envWithoutProxy(os.Environ())
		out, err := connCmd.CombinedOutput()
		t.Logf("[2] 锁定 profile 连本机 127.0.0.1:17890: err=%v\n%s", err, out)
		if err != nil {
			t.Fatalf("本机代理端口应被放行，连接却失败: %v\n%s", err, out)
		}
	}

	// 4) DNS 对照：去掉 unix-socket 放行后应出现 "Could not resolve host"，
	//    反证完整规则里 DNS 没被误杀（可选断言，DNS 缓存可能让它不稳定，只记录）。
	noDNS := strings.Replace(locked, "(allow network-outbound (remote unix-socket))\n", "", 1)
	dnsOut, _ := runCurl(noDNS)
	t.Logf("[4] 去掉 unix-socket 放行后直连: %s", dnsOut)
	if strings.Contains(strings.ToLower(dnsOut), "could not resolve host") {
		t.Log("去掉 unix-socket 后出现 Could not resolve host：证明该放行规则是 DNS 所需")
	} else {
		t.Log("去掉 unix-socket 后未出现 Could not resolve host（DNS 缓存/系统解析器差异），不强制断言")
	}
}
