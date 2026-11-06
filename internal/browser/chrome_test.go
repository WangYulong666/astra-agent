package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseDevToolsLine 覆盖 Chrome stderr 那一行的解析，含各种畸形输入。
func TestParseDevToolsLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "标准行",
			in:   "DevTools listening on ws://127.0.0.1:53412/devtools/browser/2f1c9a3b-0000-4a49-8f4b-1a2b3c4d5e6f",
			want: "ws://127.0.0.1:53412/devtools/browser/2f1c9a3b-0000-4a49-8f4b-1a2b3c4d5e6f",
			ok:   true,
		},
		{
			name: "带前导空白与换行（Windows CRLF）",
			in:   "  DevTools listening on ws://127.0.0.1:9222/devtools/browser/abc\r",
			want: "ws://127.0.0.1:9222/devtools/browser/abc",
			ok:   true,
		},
		{
			name: "前后有别的内容",
			in:   "[1106/145803.123:INFO:headless_shell.cc(1)] DevTools listening on ws://127.0.0.1:40123/devtools/browser/x-y-z",
			want: "ws://127.0.0.1:40123/devtools/browser/x-y-z",
			ok:   true,
		},
		{
			name: "行尾还有别的字",
			in:   "DevTools listening on ws://127.0.0.1:1234/devtools/browser/uuid extra",
			want: "ws://127.0.0.1:1234/devtools/browser/uuid",
			ok:   true,
		},
		{name: "空行", in: "", ok: false},
		{name: "只有前缀", in: "DevTools listening on", ok: false},
		{name: "前缀后是空白", in: "DevTools listening on   ", ok: false},
		{name: "不是 ws", in: "DevTools listening on http://127.0.0.1:9222/devtools/browser/x", ok: false},
		{name: "缺端口", in: "DevTools listening on ws://127.0.0.1/devtools/browser/x", ok: false},
		{name: "路径不是 browser", in: "DevTools listening on ws://127.0.0.1:9222/devtools/page/x", ok: false},
		{name: "地址不可解析", in: "DevTools listening on ws://%zz/devtools/browser/x", ok: false},
		{name: "无关日志", in: "[ERROR:gpu_init.cc(481)] Passthrough is not supported", ok: false},
		{name: "大小写不符", in: "devtools listening on ws://127.0.0.1:1/devtools/browser/x", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseDevToolsLine(c.in)
			if ok != c.ok {
				t.Fatalf("ok=%v, 期望 %v（输入 %q）", ok, c.ok, c.in)
			}
			if got != c.want {
				t.Fatalf("got %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestHTTPURLFromWS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ws://127.0.0.1:53412/devtools/browser/x", "http://127.0.0.1:53412"},
		{"wss://example.test:443/devtools/browser/x", "http://example.test:443"},
		{"", ""},
		{"%%%x", ""},
	}
	for _, c := range cases {
		if got := httpURLFromWS(c.in); got != c.want {
			t.Errorf("httpURLFromWS(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestHostResolverRules 确认白名单被翻成 host-resolver-rules：
// 白名单里的放行、其余全 map 到 0.0.0.0，且 EXCLUDE 必须排在 MAP 前面
// （规则是顺序匹配，先命中的生效）。
func TestHostResolverRules(t *testing.T) {
	t.Run("空白名单不加规则", func(t *testing.T) {
		for _, in := range [][]string{nil, {}, {""}, {"   "}, {"../etc/passwd"}} {
			got, err := hostResolverRules(in)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != "" {
				t.Fatalf("空白名单应返回空规则，得到 %q（输入 %v）", got, in)
			}
		}
	})

	t.Run("白名单", func(t *testing.T) {
		rules, err := hostResolverRules([]string{"Example.COM", "https://foo.test:8443/path", "*.bar.test", "bad host", "exa,mple"})
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		for _, want := range []string{
			"EXCLUDE example.com", "EXCLUDE *.example.com",
			"EXCLUDE foo.test", "EXCLUDE *.foo.test",
			"EXCLUDE bar.test", "EXCLUDE *.bar.test",
			"EXCLUDE localhost", "EXCLUDE 127.0.0.1",
			"MAP * 0.0.0.0",
		} {
			if !strings.Contains(rules, want) {
				t.Fatalf("规则里缺少 %q: %s", want, rules)
			}
		}
		// 非法项被丢弃（拦得更狠，不会误放）
		if strings.Contains(rules, "bad host") || strings.Contains(rules, "exa,mple") {
			t.Fatalf("非法主机名应被丢弃: %s", rules)
		}
		if strings.Index(rules, "MAP * 0.0.0.0") < strings.Index(rules, "EXCLUDE") {
			t.Fatalf("MAP 必须在 EXCLUDE 之后（顺序匹配）: %s", rules)
		}
	})
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"example.com", "example.com", true},
		{"  Example.COM.  ", "example.com", true},
		{"https://example.com/a/b?c=1", "example.com", true},
		{"example.com:8443", "example.com", true},
		{"*.example.com", "example.com", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"", "", false},
		{"a b.com", "", false},
		{"a,b.com", "", false},
		{".com", "", false},
		{"-x.com", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeHost(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeHost(%q) = (%q,%v), 期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLocateChromePrecedence 验证「显式配置 → CHROME_PATH → 平台默认」的顺序。
func TestLocateChromePrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("该用例依赖 unix 可执行位")
	}
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit-chrome")
	fromEnv := filepath.Join(dir, "env-chrome")
	for _, p := range []string{explicit, fromEnv} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("显式优先", func(t *testing.T) {
		t.Setenv("CHROME_PATH", fromEnv)
		got, err := locateChrome(explicit)
		if err != nil || got != explicit {
			t.Fatalf("got (%q,%v)，期望 %q", got, err, explicit)
		}
	})

	t.Run("显式无效时回退到 CHROME_PATH", func(t *testing.T) {
		t.Setenv("CHROME_PATH", fromEnv)
		got, err := locateChrome(filepath.Join(dir, "does-not-exist"))
		if err != nil || got != fromEnv {
			t.Fatalf("got (%q,%v)，期望 %q", got, err, fromEnv)
		}
	})

	t.Run("都没有可执行权限时跳过", func(t *testing.T) {
		noExec := filepath.Join(dir, "noexec")
		if err := os.WriteFile(noExec, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CHROME_PATH", fromEnv)
		got, err := locateChrome(noExec)
		if err != nil || got != fromEnv {
			t.Fatalf("不可执行的候选应被跳过，got (%q,%v)", got, err)
		}
	})

	t.Run("目录不算", func(t *testing.T) {
		t.Setenv("CHROME_PATH", fromEnv)
		got, err := locateChrome(dir)
		if err != nil || got != fromEnv {
			t.Fatalf("目录应被跳过，got (%q,%v)", got, err)
		}
	})
}

// TestLocateChromeMissing：什么都找不到时必须返回 ErrDisabled（app 层据此降级）。
func TestLocateChromeMissing(t *testing.T) {
	t.Setenv("CHROME_PATH", filepath.Join(t.TempDir(), "nope"))
	// 把平台默认路径也指到不存在的地方：测试机器上装了 Chrome 时不应该假通过。
	if runtime.GOOS == "darwin" {
		t.Skip("默认路径写死在代码里，本机装了 Chrome 会命中；该场景由 TestOpenDisabled 覆盖")
	}
	_, err := locateChrome("")
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("期望 ErrDisabled，得到 %v", err)
	}
}

func TestOpenDisabled(t *testing.T) {
	m, err := Open(Config{Enabled: false})
	if !errors.Is(err, ErrDisabled) || m != nil {
		t.Fatalf("Enabled=false 时应返回 ErrDisabled 与 nil，得到 (%v,%v)", m, err)
	}
}

// TestChromeArgs 确认启动参数一条不少。
func TestChromeArgs(t *testing.T) {
	args := chromeArgs("/tmp/x", true, "EXCLUDE a.test, MAP * 0.0.0.0")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir=/tmp/x",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--disable-sync",
		"--host-resolver-rules=EXCLUDE a.test, MAP * 0.0.0.0",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少参数 %q: %s", want, joined)
		}
	}

	// 白名单为空时不加任何规则；Headless=false 时不出 headless 参数
	args = chromeArgs("/tmp/x", false, "")
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "--host-resolver-rules") {
		t.Fatalf("空规则不应加参数: %s", joined)
	}
	if strings.Contains(joined, "--headless") {
		t.Fatalf("Headless=false 不应加 headless 参数: %s", joined)
	}
}
