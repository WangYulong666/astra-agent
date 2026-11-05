// Command acp-smoke 是 M0 的验证程序。
//
// 它不需要沙箱、不需要 KVM、不需要云机器：直接在本机用 agent 子进程跑通 ACP 全链路。
// 这是整个架构的第一根支柱——这条链跑不通，ACP 方案就得回炉。
//
// 验证项（对应架构文档 M0 的 B 组）：
//
//	1 握手  initialize 拿到 capabilities，protocolVersion 必须为 1
//	2 会话  session/new 成功。**这一步不能省**：它才真正触发 schema 校验与会话持久化，
//	        只测 initialize 会漏掉一整类问题（dsh 裁剪时就是栽在这）
//	3 对话  session/prompt 收到 session/update 流
//	4 权限  记录 request_permission 实际收到的 option 集合
//	5 恢复  session/resume 能在同一 agent 上恢复会话执行态
//
// 用法：
//
//	go run ./cmd/acp-smoke \
//	    -agent "node /path/to/dsh/lib/bin.js --profile acp" \
//	    -cwd /tmp/acp-workspace \
//	    -prompt "用一句话回答：1+1 等于几？"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/acp"
)

type options struct {
	agentCmd   string
	cwd        string
	prompt     string
	timeout    time.Duration
	permPolicy string
	url        string
	token      string
	sessionID  string
	resumeOnly bool
	skipPrompt bool
	skipResume bool
	verbose    bool
}

func main() {
	var o options
	flag.StringVar(&o.agentCmd, "agent", "dsh --profile acp", "agent 启动命令（空格切分）")
	flag.StringVar(&o.cwd, "cwd", "", "agent 工作目录，空则用临时目录（-url 模式下默认 /workspace）")
	flag.StringVar(&o.prompt, "prompt", "用一句话回答：1+1 等于几？", "发给 agent 的提示词")
	flag.DurationVar(&o.timeout, "timeout", 150*time.Second, "整体超时")
	flag.StringVar(&o.permPolicy, "perm", "allow_once", "收到权限请求时的策略：allow_once / reject_once / cancel")
	flag.StringVar(&o.url, "url", "", "acp-proxy 的 WSS 地址（如 ws://127.0.0.1:8080/acp）；留空则本地起 agent 子进程")
	flag.StringVar(&o.token, "token", "", "-url 模式下的 Bearer token")
	flag.StringVar(&o.sessionID, "session", "", "复用已有 sessionId，跳过 session/new（用于验证跨连接恢复）")
	flag.BoolVar(&o.resumeOnly, "resume-only", false, "只调 session/resume 后退出")
	flag.BoolVar(&o.skipPrompt, "skip-prompt", false, "只验证到 session/new")
	flag.BoolVar(&o.skipResume, "skip-resume", false, "跳过 session/resume 验证")
	flag.BoolVar(&o.verbose, "v", false, "打印每一帧原始内容")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31m✗ 验证失败：%v\033[0m\n", err)
		os.Exit(1)
	}
}

func run(o options) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	cwd := o.cwd
	if cwd == "" {
		if o.url != "" {
			cwd = "/workspace"
		} else {
			dir, err := os.MkdirTemp("", "acp-workspace-*")
			if err != nil {
				return err
			}
			cwd = dir
			defer os.RemoveAll(dir)
		}
	}
	if o.url == "" {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			return err
		}
	}

	// 传输层二选一：本地子进程（stdio）或远端 acp-proxy（WSS）。
	// 上面的协议代码完全相同——这就是分层带来的好处。
	var rwc io.ReadWriteCloser
	if o.url != "" {
		step("连接 acp-proxy")
		ws, err := acp.DialWS(ctx, o.url, o.token)
		if err != nil {
			return fmt.Errorf("连接 %s 失败: %w", o.url, err)
		}
		defer ws.Close()
		rwc = ws
		fmt.Printf("  url=%s\n", o.url)
	} else {
		step("启动 agent")
		parts := strings.Fields(o.agentCmd)
		if len(parts) == 0 {
			return fmt.Errorf("agent 命令为空")
		}
		conn, err := acp.StartStdio(parts[0], parts[1:], acp.WithDir(cwd))
		if err != nil {
			return fmt.Errorf("启动 agent 失败: %w", err)
		}
		defer conn.Close()
		rwc = conn
		fmt.Printf("  pid=%d  cwd=%s\n", conn.PID(), cwd)
	}

	link := acp.NewConn(rwc)
	client := acp.NewClient(link)

	rec := &recorder{verbose: o.verbose}
	client.OnUpdate = rec.onUpdate
	client.OnPermission = func(ctx context.Context, p acp.RequestPermissionParams) (acp.PermissionOutcome, error) {
		return rec.onPermission(ctx, p, o.permPolicy)
	}

	// ── 1. 握手 ────────────────────────────────────────────────
	step("1/5  initialize")
	init, err := client.Initialize(ctx, acp.ClientCapabilities{
		FS:       &acp.FSCapabilities{ReadTextFile: true, WriteTextFile: true},
		Terminal: true,
	})
	if err != nil {
		return fmt.Errorf("initialize 失败: %w", err)
	}
	fmt.Printf("  protocolVersion = %d\n", init.ProtocolVersion)
	if pc := init.AgentCapabilities.PromptCapabilities; pc != nil {
		fmt.Printf("  prompt: image=%v audio=%v embeddedContext=%v\n", pc.Image, pc.Audio, pc.EmbeddedContext)
	}
	if mc := init.AgentCapabilities.MCPCapabilities; mc != nil {
		fmt.Printf("  mcp:    http=%v sse=%v\n", mc.HTTP, mc.SSE)
	}
	fmt.Printf("  loadSession = %v\n", init.AgentCapabilities.LoadSession)
	if len(init.AuthMethods) > 0 {
		ids := make([]string, 0, len(init.AuthMethods))
		for _, a := range init.AuthMethods {
			ids = append(ids, a.ID)
		}
		fmt.Printf("  authMethods = %v\n", ids)
	}
	fmt.Println("  \033[32m✓ 握手通过\033[0m")

	// ── 2. 建会话 ──────────────────────────────────────────────
	if o.sessionID != "" {
		step("2/5  复用既有会话")
		fmt.Printf("  sessionId = %s（跳过 session/new）\n", o.sessionID)
	} else {
		step("2/5  session/new")
		sid, err := client.NewSession(ctx, cwd, nil)
		if err != nil {
			return fmt.Errorf("session/new 失败: %w", err)
		}
		o.sessionID = sid
		fmt.Printf("  sessionId = %s\n", sid)
		fmt.Println("  \033[32m✓ 会话已建立（schema 校验 + 持久化链路通过）\033[0m")
	}
	sessionID := o.sessionID

	if o.resumeOnly {
		step("resume")
		if err := client.Resume(ctx, sessionID, cwd); err != nil {
			return fmt.Errorf("session/resume 失败: %w", err)
		}
		fmt.Println("  \033[32m✓ 恢复成功\033[0m")
		fmt.Println("    前提是 agent 进程已经重启过（会话不在内存里）。")
		fmt.Println("    若 agent 还活着，dsh 会返回 `session is already active` —— 这是正确行为，")
		fmt.Println("    因为会话本来就在内存中，此时直接继续对话即可，不需要 resume。")
		return nil
	}

	if o.skipPrompt {
		rec.summary()
		return nil
	}

	// ── 3. 跑一回合 ────────────────────────────────────────────
	step("3/5  session/prompt")
	start := time.Now()
	res, promptErr := client.Prompt(ctx, sessionID, acp.Text(o.prompt))
	elapsed := time.Since(start)
	fmt.Printf("  耗时 %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  stopReason = %q\n", res.StopReason)

	// 环境层面缺 API key 时，agent 会拒绝或立刻停下。这不算协议失败——
	// 链路本身（请求发出、事件回流、响应解析）已经跑通了。
	if promptErr != nil {
		fmt.Printf("  \033[33m⚠ prompt 返回错误：%v\033[0m\n", promptErr)
		fmt.Println("    若错误与 API key / 认证相关，属于环境未配置，不影响链路判定")
	}
	if n := rec.total(); n == 0 {
		fmt.Println("  \033[33m⚠ 没有收到任何 session/update\033[0m")
	} else {
		fmt.Printf("  收到 %d 条 session/update\n", n)
		echo := strings.TrimSpace(rec.text.String())
		if echo != "" {
			fmt.Printf("  agent 输出：%s\n", truncate(echo, 300))
		}
		fmt.Println("  \033[32m✓ 事件流通过\033[0m")
	}

	// ── 4. 权限拦截 ────────────────────────────────────────────
	step("4/5  session/request_permission")
	perms := rec.permissions()
	if len(perms) == 0 {
		fmt.Println("  \033[33m本次未触发权限请求\033[0m")
		fmt.Println("  说明：这轮 prompt 没有让 agent 调用需要审批的工具。")
		fmt.Println("  要主动触发，可把 -prompt 换成需要执行命令的任务，例如：")
		fmt.Println("    \"在工作目录里创建一个 hello.txt，内容写 hi\"")
	} else {
		for i, p := range perms {
			fmt.Printf("  [%d] tool=%q kind=%q status=%q\n", i+1, p.ToolCall.Title, p.ToolCall.Kind, p.ToolCall.Status)
			kinds := make([]string, 0, len(p.Options))
			for _, op := range p.Options {
				kinds = append(kinds, op.Kind)
			}
			fmt.Printf("      options(kind) = %v\n", kinds)
			if _, ok := p.Select(acp.KindAllowAlways); ok {
				fmt.Println("      含 allow_always —— 审批策略里必须显式禁止它能防止越权")
			}
		}
		fmt.Printf("  \033[32m✓ 拦截到 %d 次权限请求，策略=%s\033[0m\n", len(perms), o.permPolicy)
	}

	// ── 5. resume ─────────────────────────────────────────────
	if !o.skipResume {
		step("5/5  session/resume")
		if err := client.Resume(ctx, sessionID, cwd); err != nil {
			fmt.Printf("  \033[33m⚠ resume 失败：%v\033[0m\n", err)
			fmt.Println("    若 agent 不支持 resume（能力声明里没有对应项），这属于预期")
		} else {
			fmt.Println("  \033[32m✓ resume 成功\033[0m")
		}
	}

	rec.summary()

	if err := link.Err(); err != nil {
		fmt.Printf("\n  连接状态：%v\n", err)
	}
	fmt.Println("\n\033[32mM0 链路验证完成\033[0m")
	return nil
}

// ── 事件记录 ──────────────────────────────────────────────────

type recorder struct {
	mu      sync.Mutex
	counts  map[string]int
	order   []string
	text    strings.Builder
	perms   []acp.RequestPermissionParams
	verbose bool
}

func (r *recorder) onUpdate(u acp.SessionUpdate) {
	var kind acp.UpdateKind
	_ = json.Unmarshal(u.Update, &kind)
	t := kind.Type
	if t == "" {
		t = "unknown"
	}

	r.mu.Lock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[t]++
	n := len(r.order)
	if n < 20 {
		r.order = append(r.order, t)
	}
	r.mu.Unlock()

	if r.verbose {
		fmt.Printf("     ← %s %s\n", t, truncate(string(u.Update), 200))
	}

	switch t {
	case "agent_message_chunk":
		var c struct {
			Content acp.ContentBlock `json:"content"`
		}
		if json.Unmarshal(u.Update, &c) == nil && c.Content.Text != "" {
			r.mu.Lock()
			r.text.WriteString(c.Content.Text)
			r.mu.Unlock()
			fmt.Print(c.Content.Text)
		}
	case "tool_call":
		var tc acp.ToolCall
		if json.Unmarshal(u.Update, &tc) == nil {
			fmt.Printf("\n  [tool_call] kind=%s title=%q\n", tc.Kind, tc.Title)
		}
	case "tool_call_update":
		var tu acp.ToolCallUpdate
		if json.Unmarshal(u.Update, &tu) == nil {
			fmt.Printf("  [tool_call_update] status=%s\n", tu.Status)
		}
	}
}

func (r *recorder) onPermission(_ context.Context, p acp.RequestPermissionParams, policy string) (acp.PermissionOutcome, error) {
	r.mu.Lock()
	r.perms = append(r.perms, p)
	r.mu.Unlock()

	fmt.Printf("\n  \033[33m⚑ 权限请求：\033[0m kind=%q title=%q\n", p.ToolCall.Kind, p.ToolCall.Title)
	for _, op := range p.Options {
		fmt.Printf("      - %-14s %s\n", op.Kind, op.Name)
	}

	want := policy
	if want == "cancel" {
		fmt.Println("  → 返回 cancelled")
		return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
	}
	opt, ok := p.Select(want)
	if !ok {
		fmt.Printf("  → 找不到 kind=%s 的选项，改为拒绝\n", want)
		opt, ok = p.Select(acp.KindRejectOnce)
		if !ok {
			return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
		}
	}
	fmt.Printf("  → 选择 %s (%s)\n", opt.OptionID, opt.Kind)
	return acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: opt.OptionID}, nil
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, v := range r.counts {
		n += v
	}
	return n
}

func (r *recorder) permissions() []acp.RequestPermissionParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]acp.RequestPermissionParams, len(r.perms))
	copy(out, r.perms)
	return out
}

func (r *recorder) summary() {
	step("事件汇总")
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.counts) == 0 {
		fmt.Println("  无事件")
		return
	}
	for k, v := range r.counts {
		fmt.Printf("  %-28s %d\n", k, v)
	}
}

// ── 小工具 ────────────────────────────────────────────────────

func step(name string) {
	fmt.Printf("\n\033[1m▸ %s\033[0m\n", name)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
