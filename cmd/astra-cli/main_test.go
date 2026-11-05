package main

import "testing"

// 命令切分是这一层最容易出错的地方（第一版就踩过：`login --email a@b`
// 里 --email 被当成命令）。这里把典型写法钉死。
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cmd  string
		sub  string
		rest []string
	}{
		{
			name: "分组命令 + 子命令",
			args: []string{"session", "list"},
			cmd:  "session", sub: "list", rest: []string{},
		},
		{
			name: "指令作为子命令词",
			args: []string{"run", "echo hi", "--wait"},
			cmd:  "run", sub: "echo hi", rest: []string{"--wait"},
		},
		{
			name: "flag 写在指令前",
			args: []string{"run", "--session", "s1", "echo hi"},
			cmd:  "run", sub: "", rest: []string{"--session", "s1", "echo hi"},
		},
		{
			name: "login 的值不能被当成命令",
			args: []string{"login", "--email", "a@b.com", "--password", "x"},
			cmd:  "login", sub: "", rest: []string{"--email", "a@b.com", "--password", "x"},
		},
		{
			name: "全局 flag 在命令前",
			args: []string{"--json", "session", "list"},
			cmd:  "session", sub: "list", rest: []string{"--json"},
		},
		{
			name: "非分组命令不吃位置参数",
			args: []string{"approve", "r1", "tc1", "--allow"},
			cmd:  "approve", sub: "", rest: []string{"r1", "tc1", "--allow"},
		},
		{
			name: "未知命令原样返回",
			args: []string{"nope"},
			cmd:  "nope", sub: "", rest: []string{"nope"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, sub, rest := splitCommand(c.args)
			if cmd != c.cmd || sub != c.sub {
				t.Fatalf("cmd/sub = %q/%q，期望 %q/%q", cmd, sub, c.cmd, c.sub)
			}
			if len(rest) != len(c.rest) {
				t.Fatalf("rest = %v，期望 %v", rest, c.rest)
			}
			for i := range rest {
				if rest[i] != c.rest[i] {
					t.Fatalf("rest[%d] = %q，期望 %q（rest=%v）", i, rest[i], c.rest[i], rest)
				}
			}
		})
	}
}
