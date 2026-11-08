package sandbox

// ASTRA_CLI_BIN 的 PATH 注入、Resume 持久化，以及「bin 不可用不应拖垮沙箱」的测试。
//
// 既有 profile/Seatbelt 测试不在这份文件里动：本次改动刻意不碰 profile()，
// 由 TestProfileCoversWorkspaceOnly / TestProfileNetworkPolicyDisabledByteIdentical
// 等原样守住「Enabled=false 逐字节一致」。

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// envValues 取出 env 里某个 key 的全部值（形如 "KEY=VALUE"）。
// 同时用于断言「恰好一条」——重复键在 exec 里是平台相关行为，必须避免。
func envValues(env []string, key string) []string {
	prefix := key + "="
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, strings.TrimPrefix(kv, prefix))
		}
	}
	return out
}

// writeExecutableCLI 造一个真带执行位的文件，返回其绝对路径。
func writeExecutableCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "astra-cli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("写可执行 CLI 文件失败: %v", err)
	}
	return bin
}

// ── 无 ASTRA_CLI_BIN：与改动前逐字节一致 ──

func TestChildEnvWithoutCLIBin(t *testing.T) {
	l := NewLocal(Options{Root: t.TempDir()})
	m := &meta{ID: "sbx", Workspace: "/tmp/astra-test-root/sbx/workspace", Home: "/tmp/astra-test-root/sbx/home"}

	// 空 Env → 结果完全确定，可与「旧 childEnv 的构造」逐字节比对。
	got := l.childEnv(m, AgentSpec{})

	want := append([]string{}, os.Environ()...)
	want = append(want,
		"HOME="+m.Home,
		"DSH_HOME="+m.Home,
		"TMPDIR="+filepath.Join(l.dir(m.ID), "tmp"),
		"ASTRA_SANDBOX_ID="+m.ID,
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("无 CLI bin 时 childEnv 必须与改动前逐字节一致：\n got=%v\nwant=%v", got, want)
	}
	if v := envValues(got, "ASTRA_CLI_BIN"); len(v) != 0 {
		t.Fatalf("无 CLI bin 时不该出现 ASTRA_CLI_BIN，实际 %v", v)
	}
	// PATH 原样保留，没被前置/改写。
	if v := envValues(got, "PATH"); len(v) != 1 || v[0] != os.Getenv("PATH") {
		t.Fatalf("无 CLI bin 时 PATH 应保持原样，实际 %v", v)
	}
}

// ── 有 ASTRA_CLI_BIN 且可执行：前置进 PATH，变量本身保留 ──

func TestChildEnvWithCLIBin(t *testing.T) {
	l := NewLocal(Options{Root: t.TempDir()})
	m := &meta{ID: "sbx", Workspace: "/tmp/astra-test-root/sbx/workspace", Home: "/tmp/astra-test-root/sbx/home"}
	bin := writeExecutableCLI(t)
	dir := filepath.Dir(bin)
	origPATH := os.Getenv("PATH")

	got := l.childEnv(m, AgentSpec{Env: map[string]string{
		"ASTRA_CLI_BIN": bin,
		"ASTRA_TOKEN":   "tok-123",
	}})

	// ASTRA_CLI_BIN 本身保留，值不变，且恰好一条。
	if v := envValues(got, "ASTRA_CLI_BIN"); len(v) != 1 || v[0] != bin {
		t.Fatalf("ASTRA_CLI_BIN 应保留为 %q（恰好一条），实际 %v", bin, v)
	}
	// PATH：dir 前置 + 保留原 PATH，用 os.PathListSeparator 拼接，恰好一条。
	want := dir + string(os.PathListSeparator) + origPATH
	if v := envValues(got, "PATH"); len(v) != 1 || v[0] != want {
		t.Fatalf("PATH 应为 %q（恰好一条），实际 %v", want, v)
	}
	// 其它注入的环境变量不丢。
	if v := envValues(got, "ASTRA_TOKEN"); len(v) != 1 || v[0] != "tok-123" {
		t.Fatalf("其它 Env 项丢失: %v", v)
	}
	// 基线变量仍在。
	for _, k := range []string{"HOME", "DSH_HOME", "TMPDIR", "ASTRA_SANDBOX_ID"} {
		if len(envValues(got, k)) == 0 {
			t.Errorf("基线变量 %s 丢失", k)
		}
	}
}

// TestCLIBinExecutable 直接锁住「可执行」判定的边界。
func TestCLIBinExecutable(t *testing.T) {
	good := writeExecutableCLI(t)
	if !cliBinExecutable(good) {
		t.Fatal("带执行位的普通文件应判为可执行")
	}

	noexec := filepath.Join(t.TempDir(), "astra-cli")
	if err := os.WriteFile(noexec, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cliBinExecutable(noexec) {
		t.Error("无执行位的文件不应判为可执行")
	}
	if cliBinExecutable(filepath.Join(t.TempDir(), "missing")) {
		t.Error("不存在的文件不应判为可执行")
	}
	if cliBinExecutable(t.TempDir()) {
		t.Error("目录不应判为可执行")
	}
}

// ── bin 不可用：只跳过 PATH 注入，绝不让 spawn 失败 ──

// TestChildEnvCLIBinUnusableDoesNotInjectPath 覆盖「bin 不存在 / 无执行位 / 是目录」
// 三种不可用情形：childEnv 必须正常返回（不 panic、不注入坏路径），这样 spawn 里
// `cmd.Env = l.childEnv(...)` 就照常进行，坏 CLI bin 不会让整个沙箱起不来。
func TestChildEnvCLIBinUnusableDoesNotInjectPath(t *testing.T) {
	l := NewLocal(Options{Root: t.TempDir()})
	m := &meta{ID: "sbx", Workspace: "/tmp/astra-test-root/sbx/workspace", Home: "/tmp/astra-test-root/sbx/home"}
	origPATH := os.Getenv("PATH")

	nonexec := filepath.Join(t.TempDir(), "astra-cli")
	if err := os.WriteFile(nonexec, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		bin  string
	}{
		{"不存在", filepath.Join(t.TempDir(), "no-such-cli")},
		{"无执行位", nonexec},
		{"是目录", t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := l.childEnv(m, AgentSpec{Env: map[string]string{"ASTRA_CLI_BIN": tc.bin}})
			// 能走到这里就说明没 panic —— spawn 不受影响。
			if v := envValues(got, "PATH"); len(v) != 1 || v[0] != origPATH {
				t.Fatalf("bin 不可用时 PATH 应保持原样（不注入），实际 %v", v)
			}
			// ASTRA_CLI_BIN 仍保留：让 skillhub 读得到并把它标成 unavailable。
			if v := envValues(got, "ASTRA_CLI_BIN"); len(v) != 1 || v[0] != tc.bin {
				t.Fatalf("ASTRA_CLI_BIN 应保留，实际 %v", v)
			}
		})
	}
}

// TestCreateWithUnusableCLIBinStillSucceeds（集成）：真起一次 Create，证明一个
// 不可用的 ASTRA_CLI_BIN 不会让 spawn / 沙箱创建失败，且 bin 路径仍被落盘。
func TestCreateWithUnusableCLIBinStillSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("集成测试，-short 下跳过")
	}
	// 本会话的 go test 进程本身可能被沙箱化（macOS 沙箱不能嵌套），此时
	// sandbox-exec 给 acp-proxy apply profile 会返回 Operation not permitted，
	// 代理根本起不来、health 永远 refused——那是环境限制，与 CLI bin 无关。
	// 用平台自带的降级开关绕开，专门验证「坏 CLI bin 不影响 Create/spawn」。
	t.Setenv("ASTRA_NO_SEATBELT", "1")

	proxy := buildProxy(t)
	root := t.TempDir()
	l := NewLocal(Options{Root: root, ProxyBin: proxy})
	ctx := context.Background()

	badBin := filepath.Join(t.TempDir(), "no-such-astra-cli")
	sb, err := l.Create(ctx, CreateRequest{
		ID: "cli-bad",
		Agent: AgentSpec{
			Command: []string{"cat"},
			Env:     map[string]string{"ASTRA_CLI_BIN": badBin},
		},
	})
	if err != nil {
		t.Fatalf("CLI bin 不可用不应让整个沙箱起不来（spawn 不受影响），实际: %v", err)
	}
	if sb == nil {
		t.Fatal("Create 返回了 nil 句柄")
	}
	defer func() { _ = l.Destroy(ctx, "cli-bad") }()

	// CLIBin 仍要落盘：Resume / 幂等重建靠它把路径找回来。
	m, err := l.readMeta("cli-bad")
	if err != nil {
		t.Fatalf("读 meta: %v", err)
	}
	if m.CLIBin != badBin {
		t.Fatalf("meta.CLIBin 应持久化 %q，实际 %q", badBin, m.CLIBin)
	}
}

// ── Resume：不带 Env 重启，CLI 路径不能丢 ──

// TestChildEnvResumeKeepsCLIPath 复刻 Resume 的关键路径：Resume 用
// spawn(s.m, AgentSpec{Command: s.m.Agent}) 重启、完全不带 Env。CLI bin 必须
// 像 NetworkPolicy 一样先落盘进 meta.json，childEnv 才能从 meta.CLIBin 取回，
// 否则 pause→resume 后 PATH 注入会静默消失、沙箱里的 astra-cli 又解析不到。
func TestChildEnvResumeKeepsCLIPath(t *testing.T) {
	root := t.TempDir()
	bin := writeExecutableCLI(t)
	dir := filepath.Dir(bin)

	l := NewLocal(Options{Root: root})
	m := &meta{
		ID: "resume", Port: 41234, Token: "t",
		Workspace: filepath.Join(root, "resume", "workspace"),
		Home:      filepath.Join(root, "resume", "home"),
		Agent:     []string{"cat"},
		State:     StateRunning,
		Strict:    false,
		CLIBin:    bin,
	}
	if err := os.MkdirAll(l.dir(m.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.writeMeta(m); err != nil {
		t.Fatalf("写 meta: %v", err)
	}

	// 模拟 Acquire / Resume 读回的 meta。
	back, err := l.readMeta(m.ID)
	if err != nil {
		t.Fatalf("读回 meta: %v", err)
	}
	if back.CLIBin != bin {
		t.Fatalf("CLIBin 落盘/读回丢失: got %q want %q", back.CLIBin, bin)
	}

	// Resume 的 spawn 参数：只有 Command，没有 Env / NetworkPolicy。
	got := l.childEnv(back, AgentSpec{Command: back.Agent})

	want := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	if v := envValues(got, "PATH"); len(v) != 1 || v[0] != want {
		t.Fatalf("Resume 后 PATH 注入丢失: %v（应为 %q）", v, want)
	}
	if v := envValues(got, "ASTRA_CLI_BIN"); len(v) != 1 || v[0] != bin {
		t.Fatalf("Resume 后 ASTRA_CLI_BIN 丢失: %v", v)
	}
}
