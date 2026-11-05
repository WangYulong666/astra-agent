package main

import (
	"context"
	"fmt"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const sandboxHelp = `用法：
  admin-cli sandbox list
  admin-cli sandbox kill <沙箱ID> [--yes]

说明：kill 只销毁运行环境，不删会话记录（历史对话与审批记录会保留）。
`

func (a *app) cmdSandbox(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.sandboxList(args)
	case "kill", "rm":
		return a.sandboxKill(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, sandboxHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "sandbox 没有子命令 %q\n\n%s", sub, sandboxHelp)
		return 2
	}
}

func (a *app) sandboxList(args []string) int {
	fs := a.newFlagSet("sandbox list")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	list, err := c.AdminSandboxes(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(list, func() error {
		t := clikit.Table{
			Header: []string{"ID", "STATE", "PROVIDER", "PID", "AGE", "SESSION", "TITLE"},
			Right:  map[int]bool{3: true},
		}
		now := time.Now()
		for _, s := range list {
			age := "-"
			if !s.CreatedAt.IsZero() {
				age = now.Sub(s.CreatedAt).Round(time.Second).String()
			}
			pid := "-"
			if s.PID > 0 {
				pid = fmt.Sprintf("%d", s.PID)
			}
			t.Rows = append(t.Rows, []string{
				s.ID, s.State, s.Provider, pid, age, dash(s.SessionID), clikit.Trunc(s.SessionTitle, 30),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 个沙箱\n", len(list))
		return nil
	})
}

func (a *app) sandboxKill(args []string) int {
	fs := a.newFlagSet("sandbox kill")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli sandbox kill <沙箱ID> [--yes]"); err != nil {
		return a.fail(err)
	}
	if !a.confirm(fmt.Sprintf("强杀沙箱 %s 会终止其中正在运行的 agent（会话记录保留），继续？", pos[0])) {
		fmt.Fprintln(a.out, "已取消")
		return 1
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if err := c.AdminKillSandbox(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.jsonOut(map[string]string{"killed": pos[0]}, nil)
	}
	fmt.Fprintf(a.out, "已强杀沙箱 %s（会话记录保留）\n", pos[0])
	return 0
}
