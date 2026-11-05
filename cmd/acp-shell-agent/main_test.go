package main

import (
	"strings"
	"testing"
)

func TestNormalizeCommand(t *testing.T) {
	cases := map[string]string{
		"ls -la":         "ls -la",
		"  ls -la  ":     "ls -la",
		"!ls":            "ls",
		"! ls":           "ls",
		"$ echo hi":      "echo hi",
		"> cat a.txt":    "cat a.txt",
		"echo '!x'":      "echo '!x'", // 只剥前缀，不动内部
		"":               "",
		"   ":            "",
		"!!not-a-prefix": "!not-a-prefix",
		"$\techo tab":    "echo tab",
		"多行\n命令":         "多行\n命令",
	}
	for in, want := range cases {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// childEnv 必须只透传安全变量，并把 HOME 指到沙箱内。
// 这条不是洁癖：HOME 指向宿主时，`npm`/`git` 会往宿主家目录写缓存，
// 在 workspace-write 下被内核拒绝，用户看到的是莫名其妙的 EPERM。
func TestChildEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SECRET_TOKEN", "should-not-leak")

	env := childEnv("/tmp/ws")
	joined := strings.Join(env, "\n")

	if !strings.Contains(joined, "HOME=/tmp/ws") {
		t.Errorf("HOME 没有指到工作目录:\n%s", joined)
	}
	if strings.Contains(joined, "SECRET_TOKEN") {
		t.Error("环境变量泄漏：不该把宿主任意变量透传给 agent 执行的命令")
	}
	if !strings.Contains(joined, "PATH=") {
		t.Error("PATH 丢了，命令会找不到")
	}
}
