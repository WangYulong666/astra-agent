// acp-shell-agent 把 prompt 当成 shell 命令执行，用于本地开发与演示。
//
// 它存在的理由：控制面（runner / 事件流 / SSE / 审批）必须能在没有模型凭据的
// 机器上跑通。dsh 没有 DeepSeek API key 就只会返回 -32603，那样任何上层改动都
// 得先解决凭据才能验证，代价太大。这个 agent 让「prompt → 真实进程 → 流式输出」
// 这条路径完全可用，且在沙箱里真的执行命令 —— 不是假数据。
//
// 协议行为与真实 agent 一致：
//
//	session/prompt
//	  ├─ tool_call(kind=execute)      命令即将执行
//	  ├─ session/request_permission   会阻塞等审批（策略在 client 侧）
//	  ├─ agent_message_chunk ×N       stdout/stderr 逐行流式回传
//	  └─ tool_call_update(completed)  带 exitCode
//
// 环境变量：
//
//	ASTRA_SHELL_STATE        有值时把会话 cwd 落盘，用于跨进程 resume
//	ASTRA_SHELL_NO_PERM      非空则跳过权限请求
//	ASTRA_SHELL_TIMEOUT_MS   单条命令超时，默认 120000
//	ASTRA_SHELL_MAX_BYTES    单条命令输出上限，默认 1048576（超了截断并说明）
//	ASTRA_SHELL_SHELL        解释器，默认 bash（取 PATH 里的）或 sh
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WangYulong666/astra-agent/internal/acp"
)

func main() {
	// ACP 走 stdout 传帧，日志必须走 stderr，否则会污染 NDJSON 流。
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	shell := os.Getenv("ASTRA_SHELL_SHELL")
	if shell == "" {
		if _, err := exec.LookPath("bash"); err == nil {
			shell = "bash"
		} else {
			shell = "sh"
		}
	}

	a := &agent{
		sessions: map[string]*session{},
		stateDir: os.Getenv("ASTRA_SHELL_STATE"),
		noPerm:   os.Getenv("ASTRA_SHELL_NO_PERM") != "",
		timeout:  envDuration("ASTRA_SHELL_TIMEOUT_MS", 120*time.Second),
		maxBytes: envInt("ASTRA_SHELL_MAX_BYTES", 1<<20),
		shell:    shell,
	}
	if a.stateDir != "" {
		if err := os.MkdirAll(a.stateDir, 0o755); err != nil {
			log.Fatalf("shell: 建状态目录失败: %v", err)
		}
	}

	conn := acp.NewConn(acp.Stdio())
	a.conn = conn
	conn.Handle("initialize", a.initialize)
	conn.Handle("session/new", a.newSession)
	conn.Handle("session/resume", a.resume)
	conn.Handle("session/prompt", a.prompt)
	conn.Handle("session/cancel", a.cancel)
	conn.Handle("session/close", a.closeSession)

	log.Printf("shell: 就绪 shell=%s timeout=%s state=%q", a.shell, a.timeout, a.stateDir)
	<-conn.Done()
}

type agent struct {
	conn *acp.Conn

	mu       sync.Mutex
	sessions map[string]*session

	stateDir string
	noPerm   bool
	timeout  time.Duration
	maxBytes int
	shell    string
}

type session struct {
	id  string
	cwd string

	mu     sync.Mutex
	cancel context.CancelFunc // 非 nil 表示有进行中的命令
}

func (a *agent) initialize(_ context.Context, _ json.RawMessage) (any, error) {
	// 能力面与 dsh 对齐（image/audio/embeddedContext 全 false，MCP 仅 http），
	// 让上层的分支判断在本地与生产走同一条路。
	return acp.InitializeResult{
		ProtocolVersion: acp.ProtocolVersion,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:        false,
			PromptCapabilities: &acp.PromptCapabilities{},
			MCPCapabilities:    &acp.MCPCapabilities{HTTP: true},
		},
		AuthMethods: []acp.AuthMethod{{ID: "shell", Name: "本地 shell（无需凭据）"}},
	}, nil
}

func (a *agent) newSession(_ context.Context, raw json.RawMessage) (any, error) {
	var p acp.NewSessionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "参数不合法: " + err.Error()}
	}
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	// 目录不存在就建：宿主侧传进来的 workspace 可能是新建的空目录。
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, &acp.RPCError{Code: -32603, Message: "建工作目录失败: " + err.Error()}
	}

	s := &session{id: newID(), cwd: cwd}
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()

	if a.stateDir != "" {
		if err := os.WriteFile(filepath.Join(a.stateDir, s.id), []byte(cwd), 0o644); err != nil {
			return nil, fmt.Errorf("shell: 持久化会话失败: %v", err)
		}
	}
	log.Printf("shell: 新建会话 %s cwd=%s", s.id, cwd)
	return acp.NewSessionResult{SessionID: s.id}, nil
}

func (a *agent) resume(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "参数不合法: " + err.Error()}
	}

	a.mu.Lock()
	s, ok := a.sessions[p.SessionID]
	if !ok && a.stateDir != "" {
		// 进程重启过：从落盘记录恢复（对应 dsh 从 /var/lib/dsh 恢复）。
		if b, err := os.ReadFile(filepath.Join(a.stateDir, p.SessionID)); err == nil {
			cwd := string(b)
			if p.Cwd != "" {
				cwd = p.Cwd
			}
			s = &session{id: p.SessionID, cwd: cwd}
			a.sessions[p.SessionID] = s
			ok = true
		}
	}
	a.mu.Unlock()

	if !ok {
		return nil, &acp.RPCError{Code: -32602, Message: "shell: 未知会话 " + p.SessionID}
	}
	log.Printf("shell: 恢复会话 %s", s.id)
	return struct{}{}, nil
}

func (a *agent) cancel(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &p)

	a.mu.Lock()
	s := a.sessions[p.SessionID]
	a.mu.Unlock()
	if s == nil {
		return nil, &acp.RPCError{Code: -32602, Message: "shell: 未知会话 " + p.SessionID}
	}

	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		log.Printf("shell: 收到取消 %s", p.SessionID)
		cancel()
	}
	return struct{}{}, nil
}

func (a *agent) closeSession(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &p)
	a.mu.Lock()
	delete(a.sessions, p.SessionID)
	a.mu.Unlock()
	return struct{}{}, nil
}

func (a *agent) prompt(ctx context.Context, raw json.RawMessage) (any, error) {
	var p acp.PromptParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "参数不合法: " + err.Error()}
	}

	a.mu.Lock()
	s := a.sessions[p.SessionID]
	a.mu.Unlock()
	if s == nil {
		return nil, &acp.RPCError{Code: -32602, Message: "shell: 未知会话 " + p.SessionID}
	}

	// 注意这里只是「可取消」，不带超时：a.timeout 是**命令**的执行上限，
	// 不该把等待人工审批的时间也算进去。之前把超时加在这里，结果审批等人
	// 超过 120s 就会以 context deadline exceeded 收场——看起来像 agent 崩了，
	// 实际只是没人点「允许」。
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		// ACP 不变量：一个会话同时只能有一个进行中的 prompt。
		return nil, &acp.RPCError{Code: -32600, Message: "shell: 该会话已有进行中的命令"}
	}
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	cmdText := normalizeCommand(joinText(p.Prompt))
	if cmdText == "" {
		return nil, &acp.RPCError{Code: -32602, Message: "shell: 空命令"}
	}
	log.Printf("shell: prompt %s: %q", p.SessionID, cmdText)

	tcID := "tc-" + newID()[:8]
	cmdJSON, _ := json.Marshal(map[string]string{"command": cmdText, "cwd": s.cwd})
	if err := a.update(s.id, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    tcID,
		"title":         cmdText,
		"kind":          "execute",
		"status":        "pending",
		"rawInput":      json.RawMessage(cmdJSON),
	}); err != nil {
		return nil, err
	}

	// 权限请求会阻塞在 client 的决策上。真实 agent 也是这个顺序：
	// 先声明要做什么，拿到许可才动手。
	if !a.noPerm {
		out, err := acp.CallAs[acp.RequestPermissionResult](runCtx, a.conn, "session/request_permission",
			acp.RequestPermissionParams{
				SessionID: s.id,
				ToolCall: acp.ToolCall{
					SessionUpdate: "tool_call",
					ToolCallID:    tcID,
					Title:         cmdText,
					Kind:          "execute",
				},
				Options: []acp.PermissionOption{
					{OptionID: "allow-once", Name: "执行一次", Kind: acp.KindAllowOnce},
					{OptionID: "allow-always", Name: "总是执行", Kind: acp.KindAllowAlways},
					{OptionID: "reject-once", Name: "拒绝", Kind: acp.KindRejectOnce},
				},
			})
		if err != nil {
			return nil, err
		}
		allowed := out.Outcome.Outcome == acp.OutcomeSelected &&
			strings.HasPrefix(out.Outcome.OptionID, "allow")
		if !allowed {
			_ = a.update(s.id, map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    tcID,
				"status":        "failed",
				"rawOutput":     json.RawMessage(`{"error":"权限被拒绝"}`),
			})
			return acp.PromptResult{StopReason: "refusal"}, nil
		}
	}

	if err := a.update(s.id, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    tcID,
		"status":        "in_progress",
	}); err != nil {
		return nil, err
	}

	execCtx, execCancel := context.WithTimeout(runCtx, a.timeout)
	defer execCancel()
	code, cancelled, truncated, runErr := a.run(execCtx, s, cmdText)

	result := map[string]any{"exitCode": code}
	switch {
	case cancelled:
		result["cancelled"] = true
	case runErr != nil:
		result["error"] = runErr.Error()
	}
	if truncated {
		result["truncated"] = true
	}
	outJSON, _ := json.Marshal(result)

	status := "completed"
	if runErr != nil || cancelled {
		status = "failed"
	}
	if err := a.update(s.id, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    tcID,
		"status":        status,
		"rawOutput":     json.RawMessage(outJSON),
	}); err != nil {
		return nil, err
	}

	switch {
	case cancelled:
		log.Printf("shell: 命令被取消")
		return acp.PromptResult{StopReason: "cancelled"}, nil
	case runErr != nil:
		return nil, &acp.RPCError{Code: -32603, Message: runErr.Error()}
	}
	log.Printf("shell: 命令结束 exit=%d", code)
	return acp.PromptResult{StopReason: "end_turn"}, nil
}

// run 执行命令并把输出逐行推给 client。
//
// stdout 与 stderr 合并成一条流：终端里它们本来就是交错的，分开反而不还原现场。
func (a *agent) run(ctx context.Context, s *session, cmdText string) (code int, cancelled, truncated bool, err error) {
	cmd := exec.Command(a.shell, "-c", cmdText)
	cmd.Dir = s.cwd
	cmd.Env = childEnv(s.cwd)
	// 独立进程组：取消时要连子进程一起杀，否则 `sleep 100 | cat` 这类会留下孤儿。
	setPgid(cmd)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return 0, false, false, fmt.Errorf("启动命令失败: %w", err)
	}

	// 等待进程，然后关写端让读循环退出。
	//
	// 这里必须「一个 goroutine 同时负责 Wait 和 Close」：如果把 Wait 的结果发到
	// channel 上、再让另一个 goroutine 读它，就会有两个读者抢同一个值——
	// 抢输的那个永远等不到，pw 就永远不关，读循环卡在 ReadString 上，整个进程假死。
	var waitErr error
	done := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		_ = pw.Close()
		close(done)
	}()

	// 取消/超时：杀整个进程组，`cmd.Wait` 随后返回。
	// 用 atomic 而不是闭包变量：这个 goroutine 与主循环是并发的，
	// 普通 bool 会构成数据竞争（-race 下必报）。
	var killed atomic.Bool
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				killTree(cmd.Process.Pid)
			}
			killed.Store(true)
		case <-stop:
		}
	}()
	defer close(stop)

	// 逐行读并发出去。整行太短会导致大量小事件，所以攒到 256 字节或换行再发。
	br := bufio.NewReaderSize(pr, 32<<10)
	var buf strings.Builder
	total := 0
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		text := buf.String()
		buf.Reset()
		return a.update(s.id, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       acp.ContentBlock{Type: "text", Text: text},
		})
	}

	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			total += len(line)
			if total > a.maxBytes {
				buf.WriteString(line)
				_ = flush()
				_ = pr.Close()
				killTree(cmd.Process.Pid)
				truncated = true
				break
			}
			buf.WriteString(line)
			// 换行即边界，或者攒够一屏再发，避免每行一个事件。
			if strings.HasSuffix(line, "\n") || buf.Len() >= 256 {
				if err := flush(); err != nil {
					return 0, true, false, err
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := flush(); err != nil {
		return 0, true, false, err
	}

	<-done
	werr := waitErr
	if killed.Load() {
		return exitCode(cmd), true, truncated, nil
	}
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			// 非零退出不是「agent 失败」，是命令结果。原样带回去。
			return ee.ExitCode(), false, truncated, nil
		}
		return exitCode(cmd), false, truncated, werr
	}
	return 0, false, truncated, nil
}

// childEnv 给子进程一个稳定环境：HOME 指到沙箱内，避免命令读到宿主 dotfile，
// 也避免 `git`/`npm` 之类往宿主 HOME 写缓存（在 workspace-write 下会被拒）。
func childEnv(cwd string) []string {
	keep := map[string]bool{
		"PATH": true, "LANG": true, "LC_ALL": true, "TERM": true,
		"TMPDIR": true, "SHELL": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"HOME="+cwd,
		"PWD="+cwd,
		"ASTRA_SANDBOX=1",
		// 免得 git 在只读位置找配置时报错
		"GIT_CONFIG_NOSYSTEM=1",
	)
	return env
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

func (a *agent) update(sid string, body map[string]any) error {
	return a.conn.Notify("session/update", map[string]any{
		"sessionId": sid,
		"update":    body,
	})
}

// normalizeCommand 去掉首尾空白和常见的前缀写法。
//
// 界面上用户习惯写 `!ls` 或 `$ ls` 表示「这是命令」，这里一并容错。
func normalizeCommand(s string) string {
	s = strings.TrimSpace(s)
	// 只剥一个前导符号，再统一 trim。按 "! " / "$ " 这种带空格的字符串逐个匹配
	// 是不够的——用户可能用 tab，也可能写成 "!ls" 不带空格。
	if s != "" && strings.ContainsRune("!$>", rune(s[0])) {
		s = strings.TrimSpace(s[1:])
	}
	return s
}

func joinText(blocks []acp.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
