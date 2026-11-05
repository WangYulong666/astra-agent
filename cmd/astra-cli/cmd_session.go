package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const sessionHelp = `用法：
  astra-cli session list                    列出会话
  astra-cli session new [标题]               新建会话
  astra-cli session rm <会话ID> [--yes]      删除会话（销毁沙箱，不可恢复）
`

// cmdSession 实现 session list|new|rm。
func (a *app) cmdSession(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.sessionList(args)
	case "new", "create":
		return a.sessionNew(args)
	case "rm", "delete", "del":
		return a.sessionRm(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, sessionHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "session 没有子命令 %q\n\n%s", sub, sessionHelp)
		return 2
	}
}

func (a *app) sessionList(args []string) int {
	fs := a.newFlagSet("session list")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(sessions, func() error {
		t := clikit.Table{
			Header: []string{"ID", "TITLE", "STATUS", "CWD", "LAST ACTIVE"},
		}
		for _, s := range sessions {
			title := s.Title
			if title == "" {
				title = "(未命名)"
			}
			t.Rows = append(t.Rows, []string{
				s.ID,
				clikit.Trunc(title, 32),
				s.Status,
				clikit.Trunc(s.Cwd, 40),
				formatTime(s.LastActiveAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 个会话\n", len(sessions))
		return nil
	})
}

func (a *app) sessionNew(args []string) int {
	fs := a.newFlagSet("session new")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	title := ""
	if len(pos) > 0 {
		title = pos[0]
	}
	s, err := c.CreateSession(context.Background(), title)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(s, func() error {
		fmt.Fprintf(a.out, "已创建会话 %s\n", s.ID)
		if s.Title != "" {
			fmt.Fprintf(a.out, "  标题：%s\n", s.Title)
		}
		fmt.Fprintf(a.out, "  工作目录：%s\n", s.Cwd)
		return nil
	})
}

func (a *app) sessionRm(args []string) int {
	fs := a.newFlagSet("session rm")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli session rm <会话ID> [--yes]"); err != nil {
		return a.fail(err)
	}
	// 删会话会连沙箱一起销毁（历史对话也会消失），属于不可逆操作——
	// 除非显式 --yes，否则必须二次确认。
	if !*yes && !a.g.json {
		if !clikit.Confirm(a.in(), a.out, fmt.Sprintf("删除会话 %s 会连同它的沙箱一起销毁，继续？", pos[0])) {
			fmt.Fprintln(a.out, "已取消")
			return 1
		}
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if err := c.DeleteSession(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]any{"deleted": pos[0]}))
	}
	fmt.Fprintf(a.out, "已删除会话 %s\n", pos[0])
	return 0
}

func (a *app) cmdApprovals(args []string) int {
	fs := a.newFlagSet("approvals")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli approvals <run-id>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	list, err := c.ListApprovals(context.Background(), pos[0])
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(list, func() error {
		t := clikit.Table{Header: []string{"TOOL-CALL-ID", "STATUS", "KIND", "TITLE", "EXPIRES"}}
		for _, ap := range list {
			t.Rows = append(t.Rows, []string{
				ap.ToolCallID, ap.Status, ap.Kind, clikit.Trunc(ap.Title, 40), formatTime(ap.ExpiresAt),
			})
		}
		t.Render(a.out)
		if len(list) == 0 {
			fmt.Fprintln(a.out, "没有待审批项")
		}
		return nil
	})
}

func (a *app) cmdApprove(args []string) int {
	fs := a.newFlagSet("approve")
	allow := fs.Bool("allow", false, "允许")
	deny := fs.Bool("deny", false, "拒绝")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 2, "astra-cli approve <run-id> <tool-call-id> --allow|--deny"); err != nil {
		return a.fail(err)
	}
	if *allow == *deny {
		return a.fail(&client.UseError{Msg: "--allow 与 --deny 必须且只能选一个"})
	}
	decision := "deny"
	if *allow {
		decision = "allow"
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if err := c.ResolveApproval(context.Background(), pos[0], pos[1], decision); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]string{
			"runId": pos[0], "toolCallId": pos[1], "decision": decision,
		}))
	}
	fmt.Fprintf(a.out, "已提交审批：%s（run %s，tool %s）\n", decision, pos[0], pos[1])
	return 0
}

// intOrZero 把 PrintJSON 的 error 折成退出码。
func intOrZero(err error) int {
	if err != nil {
		return 1
	}
	return 0
}
