package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// admin-cli 以前没有任何测试。命令切分和 flag 值声明是这一层最容易踩的坑
// （Go 的 flag.Parse 遇第一个非 flag 就停），新增 logs 分组时把典型写法钉死。

func TestSplitCommandLogs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cmd  string
		sub  string
		rest []string
	}{
		{
			name: "logs + 子命令",
			args: []string{"logs", "egress"},
			cmd:  "logs", sub: "egress", rest: []string{},
		},
		{
			name: "logs history 带位置参数",
			args: []string{"logs", "history", "r1", "--after", "5"},
			cmd:  "logs", sub: "history", rest: []string{"r1", "--after", "5"},
		},
		{
			name: "全局 flag 在命令前",
			args: []string{"--json", "logs", "activity", "r1"},
			cmd:  "logs", sub: "activity", rest: []string{"--json", "r1"},
		},
		{
			name: "值型 flag 在子命令前",
			args: []string{"--token", "abc", "logs", "usage"},
			cmd:  "logs", sub: "usage", rest: []string{"--token", "abc"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, sub, rest := splitCommand(c.args)
			if cmd != c.cmd || sub != c.sub {
				t.Fatalf("cmd/sub = %q/%q，期望 %q/%q", cmd, sub, c.cmd, c.sub)
			}
			if strings.Join(rest, "|") != strings.Join(c.rest, "|") {
				t.Fatalf("rest = %v，期望 %v", rest, c.rest)
			}
		})
	}
}

func TestLogsHelpExitCode(t *testing.T) {
	var out, errW bytes.Buffer
	if code := run([]string{"logs", "--help"}, &out, &errW, strings.NewReader("")); code != 0 {
		t.Fatalf("logs --help 应返回 0，实际 %d（stderr=%s）", code, errW.String())
	}
	if !strings.Contains(out.String(), "logs egress") {
		t.Fatalf("帮助里没有 logs egress: %s", out.String())
	}
	// 未知子命令是用法错误，退出码必须是 2，不能是执行失败 1。
	out.Reset()
	errW.Reset()
	if code := run([]string{"logs", "bogus"}, &out, &errW, strings.NewReader("")); code != 2 {
		t.Fatalf("未知子命令应返回 2，实际 %d", code)
	}
	// 主帮助也要列出新命令，否则运维不知道它存在。
	out.Reset()
	errW.Reset()
	run([]string{"--help"}, &out, &errW, strings.NewReader(""))
	if !strings.Contains(out.String(), "logs --help") {
		t.Fatal("主帮助没列出 logs")
	}
}

// `logs --json egress` 把全局 flag 放在子命令前，splitCommand 认不出 sub。
// cmdLogs 里有兜底，这个用例保证它真的生效（打到正确端点并返回 0）。
func TestLogsFlagBeforeSubcommand(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[],"limit":100}`)
	}))
	defer srv.Close()

	var out, errW bytes.Buffer
	code := run([]string{
		"logs", "--json", "egress", "--server", srv.URL, "--token", "tok",
	}, &out, &errW, strings.NewReader(""))
	if code != 0 {
		t.Fatalf("应成功，实际 %d（stderr=%s）", code, errW.String())
	}
	if gotPath != "/v1/admin/egress/log" {
		t.Fatalf("兜底没生效，打到了 %q", gotPath)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("--json 应输出空数组，实际 %q", out.String())
	}
}

// logs activity 参数互斥校验：既不给 run 也不给 --session 时必须是用法错误（2），
// 而不是发一个路径不完整的请求。
func TestLogsActivityRequiresTarget(t *testing.T) {
	var out, errW bytes.Buffer
	code := run([]string{
		"logs", "activity", "--server", "http://127.0.0.1:1", "--token", "tok",
	}, &out, &errW, strings.NewReader(""))
	if code != 2 {
		t.Fatalf("缺 run/session 应返回 2，实际 %d（stderr=%s）", code, errW.String())
	}
}
