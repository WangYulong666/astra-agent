package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const auditHelp = `用法：
  admin-cli audit tail [--follow] [--limit 50] [--action X] [--from 时间] [--to 时间]

时间格式：RFC3339（2026-11-06T10:00:00Z）或纯日期（2026-11-06，按 UTC 当天 0 点）
输出顺序：旧 → 新（最后一行是最新一条），带 --follow 时持续追加。
`

func (a *app) cmdAudit(sub string, args []string) int {
	switch sub {
	case "tail", "list", "ls":
		return a.auditTail(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, auditHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "audit 没有子命令 %q\n\n%s", sub, auditHelp)
		return 2
	}
}

func (a *app) auditTail(args []string) int {
	fs := a.newFlagSet("audit tail")
	follow := fs.Bool("follow", false, "持续跟随（Ctrl-C 退出）")
	limit := fs.Int("limit", 50, "一次拉取条数")
	action := fs.String("action", "", "只看某个 action，如 session.create")
	from := fs.String("from", "", "起始时间")
	to := fs.String("to", "", "结束时间")
	interval := fs.Duration("interval", 2*time.Second, "--follow 的轮询间隔")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "limit", "action", "from", "to", "interval"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}

	ctx := context.Background()
	q := client.AuditQuery{Limit: *limit, Action: *action, From: *from, To: *to}
	logs, err := c.AdminAudit(ctx, q)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json && !*follow {
		return a.jsonOut(reverseAudit(logs), nil)
	}

	seen := map[string]bool{}
	printed := 0
	lastSeen := time.Now().Add(-time.Minute)
	for _, l := range reverseAudit(logs) { // 服务端 DESC，翻过来变成「旧→新」
		if seen[l.ID] {
			continue
		}
		seen[l.ID] = true
		printAudit(a.out, l)
		printed++
		if l.CreatedAt.After(lastSeen) {
			lastSeen = l.CreatedAt
		}
	}
	if printed == 0 && !*follow {
		fmt.Fprintln(a.out, "（没有审计记录）")
		return 0
	}

	if !*follow {
		return 0
	}

	// --follow：轮询而不是长连接。审计表没有 SSE，而运维场景对 2s 延迟不敏感；
	// 用 from=lastSeen 增量拉，配合已见 ID 去重（同一秒可能有多条）。
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(a.err, "跟随审计（每 %s 轮询一次，Ctrl-C 退出）…\n", *interval)
	tk := time.NewTicker(*interval)
	defer tk.Stop()
	for {
		select {
		case <-sigCtx.Done():
			return 0
		case <-tk.C:
			nq := q
			nq.From = lastSeen.Add(-time.Second).UTC().Format(time.RFC3339)
			nq.Limit = 200
			newLogs, err := c.AdminAudit(sigCtx, nq)
			if err != nil {
				if sigCtx.Err() != nil {
					return 0
				}
				fmt.Fprintf(a.err, "轮询失败（继续）：%v\n", err)
				continue
			}
			for _, l := range reverseAudit(newLogs) {
				if seen[l.ID] {
					continue
				}
				seen[l.ID] = true
				printAudit(a.out, l)
			}
			for _, l := range newLogs {
				if l.CreatedAt.After(lastSeen) {
					lastSeen = l.CreatedAt
				}
			}
		}
	}
}

func reverseAudit(in []client.AuditLog) []client.AuditLog {
	out := make([]client.AuditLog, len(in))
	for i, l := range in {
		out[len(in)-1-i] = l
	}
	return out
}

// printAudit 一行一条。detail 只打前 60 字：审计的 detail 是任意 JSON，
// 打全了会把终端刷爆（比如审批记录里带整段工具入参）。
func printAudit(w io.Writer, l client.AuditLog) {
	actor := l.ActorEmail
	if actor == "" {
		actor = l.UserID
	}
	if actor == "" {
		actor = "-"
	}
	target := l.TargetType
	if l.TargetID != "" {
		target += ":" + l.TargetID
	}
	detail := ""
	if len(l.Detail) > 0 {
		detail = " " + clikit.Trunc(string(l.Detail), 60)
	}
	fmt.Fprintf(w, "%s  %-22s %-24s %-28s %s%s\n",
		l.CreatedAt.Local().Format("01-02 15:04:05"),
		l.Action, actor, clikit.Trunc(target, 28), l.IP, detail)
}
