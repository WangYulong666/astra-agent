package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const runHelp = `用法：
  astra-cli run "指令" [--session ID] [--wait] [--timeout 5m] [--title 标题]
  astra-cli run logs <run-id> [--after SEQ]     跟随事件流（Ctrl-C 退出）
  astra-cli run cancel <run-id>                 取消任务

说明：
  不带 --wait 时只提交，打印 run-id；带 --wait 时跟随事件流到终态，
  退出码 completed=0、failed/cancelled=1。流断了用 run logs 续传。
`

// cmdRun 一个入口三个形态：
//
//	run "指令"        → 提交（sub 是指令的第一个词）
//	run logs <id>     → 跟随事件流
//	run cancel <id>   → 取消
//
// 用子命令名与指令首词撞车的代价：`astra-cli run logs` 会被当成 logs 子命令。
// 这是文档里就定死的命令表，接受这个代价；真要提交「logs」这两个字，
// 加 -- 分隔或换个说法即可。
func (a *app) cmdRun(sub string, args []string) int {
	switch sub {
	case "logs", "log":
		return a.runLogs(args)
	case "cancel":
		return a.runCancel(args)
	case "":
		if wantsHelp(args) {
			fmt.Fprint(a.out, runHelp)
			return 0
		}
	}
	// sub 为空说明 flag 写在指令前面（`run --session x "指令"`）：
	// 这时指令在位置参数里，交给 runSubmit 自己拼。
	return a.runSubmit(sub, args)
}

func (a *app) runSubmit(first string, args []string) int {
	fs := a.newFlagSet("run")
	sessionID := fs.String("session", "", "复用已有会话")
	wait := fs.Bool("wait", false, "跟随事件流直到终态")
	timeout := fs.Duration("timeout", 5*time.Minute, "--wait 的最长等待时间（0 表示不设上限）")
	title := fs.String("title", "", "新建会话时的标题")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "session", "timeout", "title"))
	if done {
		return code
	}

	words := pos
	if first != "" {
		words = append([]string{first}, pos...)
	}
	prompt := strings.TrimSpace(strings.Join(words, " "))
	if prompt == "" {
		if wantsHelp(args) {
			fmt.Fprint(a.out, runHelp)
			return 0
		}
		return a.fail(&client.UseError{Msg: "指令不能为空\n用法：astra-cli run \"echo hi\" --wait"})
	}

	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()

	// 会话：--session 直接用；否则新建。新建时把指令前 40 字当标题，
	// 这样 `session list` 里能认出这是干什么的会话，不用点进去看。
	if *sessionID == "" {
		t := *title
		if t == "" {
			t = clikit.Trunc(prompt, 40)
		}
		s, err := c.CreateSession(ctx, t)
		if err != nil {
			return a.fail(err)
		}
		*sessionID = s.ID
		fmt.Fprintf(a.err, "会话：%s\n", s.ID)
	}

	run, err := c.CreateRun(ctx, *sessionID, prompt)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json && !*wait {
		return intOrZero(clikit.PrintJSON(a.out, run))
	}
	fmt.Fprintf(a.err, "run：%s\n", run.ID)

	if !*wait {
		fmt.Fprintf(a.out, "%s\n", run.ID)
		if !a.g.json {
			fmt.Fprintf(a.err, "已提交。查看进度：astra-cli run logs %s\n", run.ID)
		}
		return 0
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if *timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	pr := &streamPrinter{out: a.out, err: a.err, verbose: a.g.verbose, quiet: a.g.json, runID: run.ID}
	status, err := a.follow(runCtx, c, run.ID, 0, pr)
	pr.finish()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(a.err, "等待超时（%s）。任务仍在服务端跑，用 astra-cli run logs %s 继续跟。\n", *timeout, run.ID)
			return 1
		}
		return a.fail(err)
	}

	final, gerr := c.GetRun(ctx, run.ID)
	if gerr == nil && final.Status != "" {
		status = final.Status
	}
	if a.g.json {
		if gerr != nil {
			final = run
			final.Status = status
		}
		_ = clikit.PrintJSON(a.out, final)
	}

	switch status {
	case "completed":
		if !a.g.json {
			fmt.Fprintf(a.err, "\n完成（completed）\n")
		}
		return 0
	case "cancelled":
		if !a.g.json {
			fmt.Fprintf(a.err, "\n已取消（cancelled）\n")
		}
		return 1
	default:
		if !a.g.json {
			msg := ""
			if final.Error != "" {
				msg = "：" + final.Error
			}
			fmt.Fprintf(a.err, "\n失败（%s）%s\n", status, msg)
		}
		return 1
	}
}

func (a *app) runLogs(args []string) int {
	fs := a.newFlagSet("run logs")
	after := fs.Int64("after", 0, "从指定 seq 之后开始（断线续传）")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "after"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli run logs <run-id> [--after SEQ]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}

	// Ctrl-C 是「停止跟随」，不是错误：ctx 取消后 follow 正常返回。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pr := &streamPrinter{out: a.out, err: a.err, verbose: a.g.verbose, quiet: a.g.json, runID: pos[0]}
	status, err := a.follow(ctx, c, pos[0], *after, pr)
	pr.finish()
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(a.err, "\n已停止跟随（run %s 仍在服务端运行）\n", pos[0])
			return 0
		}
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]string{"runId": pos[0], "status": status}))
	}
	fmt.Fprintf(a.err, "\nrun 结束：%s\n", status)
	return 0
}

func (a *app) runCancel(args []string) int {
	fs := a.newFlagSet("run cancel")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli run cancel <run-id>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if err := c.CancelRun(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]string{"cancelled": pos[0]}))
	}
	fmt.Fprintf(a.out, "已请求取消 %s\n", pos[0])
	return 0
}

// follow 订阅事件流直到终态，返回终态 status。
//
// 事件流的结束条件只有两个：收到终态事件、ctx 取消。中间断线（overflow）
// 由服务端显式发一条 overflow 事件——我们把它当警告打出来，退出码交给调用方，
// 不做自动重连：CLI 自动重连会让「进程卡住不动」很难和「真的在跑」区分开。
func (a *app) follow(ctx context.Context, c *client.Client, runID string, after int64, pr *streamPrinter) (string, error) {
	ch, err := c.RunEvents(ctx, runID, after)
	if err != nil {
		return "", err
	}
	status := ""
	for ev := range ch {
		pr.handle(ev)
		switch {
		case ev.IsTerminal():
			status = ev.Status()
		case ev.Type == client.StreamOverflow:
			fmt.Fprintln(a.err, "警告：事件流缓冲区溢出，服务端已断开，请用 run logs 续传")
		case ev.Type == client.StreamClientError:
			if ctx.Err() == nil {
				fmt.Fprintf(a.err, "警告：事件流中断（%s），用 run logs 续传\n", ev.ErrMessage())
			}
		}
	}
	if status == "" {
		// 流断了但没收到终态：退一步查一次 run 状态，避免「明明结束了却报失败」。
		if r, err := c.GetRun(context.WithoutCancel(ctx), runID); err == nil && r.Status != "" {
			status = r.Status
		}
	}
	if status == "" {
		status = "unknown"
	}
	return status, nil
}

// streamPrinter 把事件流翻译成人看的输出。
//
// 分工：message_delta 进 stdout（那是回答本身，必须能重定向到文件）；
// 工具调用、审批、错误进 stdout 也行——它们属于对话记录的一部分，
// 但「等待审批」「连接中断」这类状态提示走 stderr，不污染管道输出。
type streamPrinter struct {
	out     io.Writer
	err     io.Writer
	verbose bool
	// quiet 对应 --json：不打印人类可读内容，只留最终 JSON。
	quiet bool
	// wrote 记录是否已经往外打过正文，用于决定「工具调用前要不要补换行」。
	wrote bool
	// runID 用于把审批命令写成可以直接复制的完整命令行。
	runID string
}

func (p *streamPrinter) handle(ev client.Event) {
	if p.quiet {
		return
	}
	switch ev.Type {
	case client.EventMessageDelta:
		txt := ev.Text()
		if txt == "" {
			return
		}
		fmt.Fprint(p.out, txt)
		p.wrote = true
	case client.EventThoughtDelta:
		if p.verbose {
			fmt.Fprintf(p.err, "[思考] %s", ev.Text())
		}
	case client.EventToolStart:
		tc := ev.Tool()
		p.newlineIfNeeded()
		name := tc.Title
		if name == "" {
			name = tc.Kind
		}
		if name == "" {
			name = "tool"
		}
		summary := summarizeInput([]byte(tc.RawInput), 120)
		if summary != "" {
			fmt.Fprintf(p.out, "→ %s(%s)\n", name, summary)
		} else {
			fmt.Fprintf(p.out, "→ %s\n", name)
		}
	case client.EventToolUpdate:
		if p.verbose {
			tc := ev.Tool()
			if tc.Status != "" {
				fmt.Fprintf(p.err, "[工具] %s → %s\n", shortID(tc.ToolCallID), tc.Status)
			}
		}
	case client.EventApprovalReq:
		var ap struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Pending    bool   `json:"pending"`
		}
		_ = json.Unmarshal(ev.Data, &ap)
		if ap.Pending {
			p.newlineIfNeeded()
			fmt.Fprintf(p.out, "! 需要审批：%s\n  批准：astra-cli approve %s %s --allow\n", ap.Title, p.runID, ap.ToolCallID)
		} else if p.verbose {
			fmt.Fprintf(p.err, "[审批] %s 已按策略放行\n", ap.Title)
		}
	case client.EventApprovalResult:
		if p.verbose {
			fmt.Fprintf(p.err, "[审批] %s\n", ev.ErrMessage())
		}
	case client.EventError:
		p.newlineIfNeeded()
		fmt.Fprintf(p.out, "错误：%s\n", ev.ErrMessage())
	case client.EventState:
		st := ev.Status()
		if st == "waiting_approval" {
			fmt.Fprintln(p.err, "· 等待人工审批")
		}
	case client.EventAgentLog:
		if p.verbose {
			if m := ev.ErrMessage(); m != "" {
				fmt.Fprintf(p.err, "[agent] %s\n", m)
			}
		}
	}
}

func (p *streamPrinter) newlineIfNeeded() {
	if p.wrote {
		fmt.Fprintln(p.out)
		p.wrote = false
	}
}

// finish 收尾：回答没有以换行结束时补一个，不让 shell 提示符粘在正文后面。
func (p *streamPrinter) finish() {
	if p.quiet {
		return
	}
	if p.wrote {
		fmt.Fprintln(p.out)
		p.wrote = false
	}
}
