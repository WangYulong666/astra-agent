package main

// 「查日志」这一件事在后端散在七个端点，CLI 里以前只有 audit tail 和
// obs traces。这里收成一个 logs 分组：运维不用记住哪类日志归哪个路径，
// 一条 admin-cli logs --help 就能看到全部。
//
// 与 audit tail 的分工：audit tail 是「谁调了什么管理接口」，logs 是
// 「一次执行 / 一次出网 / 一笔用量留下了什么」。
//
// 参数解析沿用三段式：splitCommand（锚点）→ clikit.SplitFlags（值型 flag
// 必须逐个声明，漏一个就会把值当成位置参数）→ flag.FlagSet。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const logsHelp = `用法：
  admin-cli logs egress [--limit 100] [--tenant ID]     出网审计（allow/deny/审批）
  admin-cli logs history <runID> [--after N]            run 完整事件历史（JSON 版 SSE）
  admin-cli logs activity <runID> [--limit 200]         用户视角行动账本（按 run）
  admin-cli logs activity --session <ID> [--limit 200]  行动账本（按会话聚合）
  admin-cli logs children <runID>                      该 run 派生的子 agent
  admin-cli logs approvals <runID>                     待/已决的人工审批
  admin-cli logs usage [--from T] [--to T] [--limit N] [--user ID]  用量流水
  admin-cli logs billing                               本月账单
  admin-cli logs insights [--limit N] [--session ID]    失败 run 的结构化诊断与建议

时间格式：RFC3339（2026-11-06T10:00:00Z）或纯日期（2026-11-06，按 UTC 当天 0 点）。
所有命令都支持 --json 输出原始响应，便于脚本处理。
权限：egress 需要 audit:read；其余需要 session:read / usage:read。
`

// 所有带值的 flag 都必须列进来（含全局 server/token），否则值会被
// SplitFlags 当成位置参数。bool 型（--json / -v）不用列。
var logsValueFlags = clikit.BoolFlags(
	"server", "token", "limit", "tenant", "after", "run", "session", "from", "to", "user",
)

// logs 的子命令名。splitCommand 只在「命令后紧跟一个非 flag 词」时认子命令，
// 所以 `logs --json egress` 里 egress 会落进位置参数、sub 为空。这里兜一下：
// 用 SplitFlags 先把值型 flag 剥掉，再从位置参数里捞出子命令。
func isLogsSub(s string) bool {
	switch s {
	case "egress", "outbound", "history", "events", "activity", "timeline",
		"children", "child", "approvals", "approval", "usage", "billing", "insights":
		return true
	}
	return false
}

func (a *app) cmdLogs(sub string, args []string) int {
	if sub == "" {
		_, pos := clikit.SplitFlags(args, logsValueFlags)
		if len(pos) > 0 && isLogsSub(pos[0]) {
			sub = pos[0]
			for i, v := range args {
				if v == sub {
					args = append(append([]string{}, args[:i]...), args[i+1:]...)
					break
				}
			}
		}
	}
	switch sub {
	case "egress", "outbound":
		return a.logsEgress(args)
	case "history", "events":
		return a.logsHistory(args)
	case "activity", "timeline":
		return a.logsActivity(args)
	case "children", "child":
		return a.logsChildren(args)
	case "approvals", "approval":
		return a.logsApprovals(args)
	case "usage":
		return a.logsUsage(args)
	case "billing":
		return a.logsBilling(args)
	case "insights":
		return a.logsInsights(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, logsHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "logs 没有子命令 %q\n\n%s", sub, logsHelp)
		return 2
	}
}

func (a *app) logsEgress(args []string) int {
	fs := a.newFlagSet("logs egress")
	limit := fs.Int("limit", 100, "返回条数（服务端上限 500）")
	tenant := fs.String("tenant", "", "跨租户查询（需要 audit:read）")
	if _, code, done := a.parse(fs, args, logsValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.AdminEgressLog(context.Background(), *limit, *tenant)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{Header: []string{"时间", "决策", "主机", "租户", "RUN", "原因"}, Right: map[int]bool{}}
		for _, e := range items {
			t.Rows = append(t.Rows, []string{
				formatTime(e.Time), strings.ToUpper(dash(e.Decision)),
				dash(e.Host) + ":" + dash(e.Port), dash(e.TenantID), dash(e.RunID), dash(e.Reason),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 条\n", len(items))
		return nil
	})
}

func (a *app) logsHistory(args []string) int {
	fs := a.newFlagSet("logs history")
	after := fs.Int64("after", 0, "只取 seq 大于 N 的事件")
	pos, code, done := a.parse(fs, args, logsValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli logs history <runID> [--after N]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	evs, err := c.History(context.Background(), pos[0], *after)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(evs, func() error {
		for _, e := range evs {
			detail := e.Text()
			if detail == "" {
				detail = e.Status()
			}
			if detail == "" {
				detail = string(e.Data)
			}
			detail = strings.Join(strings.Fields(detail), " ")
			if len(detail) > 100 {
				detail = detail[:100] + "…"
			}
			fmt.Fprintf(a.out, "%6d  %-20s %s\n", e.Seq, e.Type, detail)
		}
		fmt.Fprintf(a.out, "共 %d 条事件\n", len(evs))
		return nil
	})
}

func (a *app) logsActivity(args []string) int {
	fs := a.newFlagSet("logs activity")
	limit := fs.Int("limit", 200, "返回条数（服务端上限 1000）")
	runID := fs.String("run", "", "按 run 查")
	sessionID := fs.String("session", "", "按会话聚合")
	pos, code, done := a.parse(fs, args, logsValueFlags)
	if done {
		return code
	}
	// 位置参数当 run id，省掉打 --run 的六个字符。
	if *runID == "" && len(pos) > 0 {
		*runID = pos[0]
	}
	if (*runID == "") == (*sessionID == "") {
		return a.fail(&client.UseError{Msg: "必须指定 run id 或 --session 之一\n" + logsHelp})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	var out client.ActivityResult
	if *sessionID != "" {
		out, err = c.SessionActivity(ctx, *sessionID, *limit)
	} else {
		out, err = c.RunActivity(ctx, *runID, *limit)
	}
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(out, func() error {
		if s := out.Summary; s != nil {
			fmt.Fprintf(a.out, "已完成 %d · 进行中 %d · 阻塞 %d · 失败 %d\n",
				s.Done, s.Running, s.Blocked, s.Failed)
			if s.Current != "" {
				fmt.Fprintf(a.out, "现在卡在：%s\n", s.Current)
			}
			fmt.Fprintln(a.out, "")
		}
		t := &clikit.Table{Header: []string{"时间", "类型", "状态", "标题", "详情"}}
		for _, e := range out.Entries {
			detail := strings.Join(strings.Fields(e.Detail), " ")
			if len(detail) > 60 {
				detail = detail[:60] + "…"
			}
			t.Rows = append(t.Rows, []string{
				formatTime(e.Time), dash(e.Kind), dash(e.Status), dash(e.Title), dash(detail),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 条\n", len(out.Entries))
		return nil
	})
}

func (a *app) logsChildren(args []string) int {
	fs := a.newFlagSet("logs children")
	pos, code, done := a.parse(fs, args, logsValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli logs children <runID>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.RunChildren(context.Background(), pos[0])
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{Header: []string{"子 RUN", "深度", "状态", "标题", "摘要", "结束"}}
		for _, ch := range items {
			summary := ch.Summary
			if summary == "" {
				summary = ch.Error
			}
			summary = strings.Join(strings.Fields(summary), " ")
			if len(summary) > 60 {
				summary = summary[:60] + "…"
			}
			t.Rows = append(t.Rows, []string{
				dash(ch.RunID), strconv.Itoa(ch.Depth), dash(ch.Status),
				dash(ch.Title), dash(summary), formatTime(ch.EndedAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 个子任务\n", len(items))
		return nil
	})
}

func (a *app) logsApprovals(args []string) int {
	fs := a.newFlagSet("logs approvals")
	pos, code, done := a.parse(fs, args, logsValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli logs approvals <runID>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.ListApprovals(context.Background(), pos[0])
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{Header: []string{"TOOL CALL", "状态", "类型", "标题", "创建", "到期"}}
		for _, ap := range items {
			t.Rows = append(t.Rows, []string{
				dash(ap.ToolCallID), dash(ap.Status), dash(ap.Kind), dash(ap.Title),
				formatTime(ap.CreatedAt), formatTime(ap.ExpiresAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 条审批\n", len(items))
		return nil
	})
}

func (a *app) logsUsage(args []string) int {
	fs := a.newFlagSet("logs usage")
	from := fs.String("from", "", "起始时间")
	to := fs.String("to", "", "结束时间")
	limit := fs.Int("limit", 100, "返回条数（服务端上限 5000）")
	user := fs.String("user", "", "只看某个用户")
	if _, code, done := a.parse(fs, args, logsValueFlags); done {
		return code
	}
	qLimit := *limit
	if qLimit > 5000 {
		qLimit = 5000
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	report, err := c.Usage(context.Background(), client.UsageQuery{
		From: *from, To: *to, Limit: qLimit, UserID: *user,
	})
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(report, func() error {
		t := &clikit.Table{Header: []string{"时间", "模型", "输入", "输出", "费用", "RUN"}, Right: map[int]bool{2: true, 3: true, 4: true}}
		for _, u := range report.Items {
			t.Rows = append(t.Rows, []string{
				formatTime(u.CreatedAt), dash(u.Model),
				strconv.FormatInt(u.InputTokens, 10), strconv.FormatInt(u.OutputTokens, 10),
				microUSD(u.CostMicroUSD), dash(u.RunID),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "合计：%d 次调用 · %d tokens · %s\n",
			report.Sum.Calls, report.Sum.TotalTokens(), microUSD(report.Sum.CostMicroUSD))
		return nil
	})
}

func (a *app) logsBilling(args []string) int {
	fs := a.newFlagSet("logs billing")
	if _, code, done := a.parse(fs, args, logsValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	out, err := c.Billing(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(out, func() error {
		for _, k := range sortedMapKeys(out) {
			fmt.Fprintf(a.out, "%-24s %v\n", k, out[k])
		}
		return nil
	})
}

// logsInsights 列失败 run 的结构化诊断。设计文档 §3 承诺的 CLI 路径，
// 后端 GET /v1/insights?limit=N&session_id=X 已存在。
func (a *app) logsInsights(args []string) int {
	fs := a.newFlagSet("logs insights")
	limit := fs.Int("limit", 50, "返回条数（服务端上限 500）")
	session := fs.String("session", "", "按会话过滤；留空看全租户（需要 owner/admin）")
	if _, code, done := a.parse(fs, args, logsValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.AdminInsights(context.Background(), *session, *limit)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{Header: []string{"时间", "RUN", "状态", "错误", "最后工具", "建议"}}
		for _, it := range items {
			t.Rows = append(t.Rows, []string{
				formatTime(it.CreatedAt), dash(it.RunID), dash(it.Status),
				clikit.Trunc(dash(it.Error), 40), dash(it.LastTool),
				clikit.Trunc(dash(it.Suggestion), 48),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 条诊断\n", len(items))
		return nil
	})
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 不引 sort：数量很小，插入排序足够且少一个 import。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
