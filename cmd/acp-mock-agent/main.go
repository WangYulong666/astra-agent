// acp-mock-agent 是一个用于测试和演示的 ACP agent 实现。
//
// 存在的理由：控制面（runner / 事件流 / SSE）的正确性不该依赖真实 LLM。
// 用 dsh 测会引入三个不稳定因素——需要 API key、模型输出不确定、上游版本会漂。
// 这个 agent 输出完全确定，但协议行为与真实 agent 一致：
//
//	initialize → session/new → session/prompt
//	  ├─ agent_thought_chunk    （思考增量）
//	  ├─ tool_call              （开始一个工具调用）
//	  ├─ 向 client 请求权限      （request_permission，会阻塞等审批）
//	  ├─ tool_call_update       （工具结束）
//	  └─ agent_message_chunk ×N （逐块输出，用于验证流式与合并）
//
// 环境变量（都可选）：
//
//	ASTRA_MOCK_DELAY_MS   每块之间的间隔，默认 12
//	ASTRA_MOCK_CHUNK      每块字符数，默认 4
//	ASTRA_MOCK_STATE      有值时把会话 id 落盘，用于验证 resume
//	ASTRA_MOCK_NO_PERM    非空则跳过权限请求
//	ASTRA_MOCK_FAIL       非空则直接返回错误（模拟「没有 API key」这类失败）
//	ASTRA_MOCK_WRITE      非空则真的往 cwd 写 notes.md（验证沙箱写限制）
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/acp"
)

func main() {
	// ACP 要求日志走 stderr。往 stdout 打一行就会污染 NDJSON 流。
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	a := &agent{
		sessions: map[string]*session{},
		delay:    envDuration("ASTRA_MOCK_DELAY_MS", 12*time.Millisecond),
		chunk:    envInt("ASTRA_MOCK_CHUNK", 4),
		stateDir: os.Getenv("ASTRA_MOCK_STATE"),
		noPerm:   os.Getenv("ASTRA_MOCK_NO_PERM") != "",
		fail:     os.Getenv("ASTRA_MOCK_FAIL") != "",
		write:    os.Getenv("ASTRA_MOCK_WRITE") != "",
	}
	if a.stateDir != "" {
		if err := os.MkdirAll(a.stateDir, 0o755); err != nil {
			log.Fatalf("mock: 建状态目录失败: %v", err)
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

	log.Printf("mock: 就绪 delay=%s chunk=%d state=%q", a.delay, a.chunk, a.stateDir)
	<-conn.Done()
}

type agent struct {
	conn *acp.Conn

	mu       sync.Mutex
	sessions map[string]*session

	delay    time.Duration
	chunk    int
	stateDir string
	noPerm   bool
	fail     bool
	write    bool
}

type session struct {
	id     string
	cwd    string
	cancel context.CancelFunc // 非 nil 表示有进行中的 prompt
}

func (a *agent) initialize(_ context.Context, _ json.RawMessage) (any, error) {
	// 能力面刻意与 dsh 一致（image/audio/embeddedContext 全 false，MCP 仅 http），
	// 这样上层代码 path 在 mock 与真实环境里走同一条分支。
	return acp.InitializeResult{
		ProtocolVersion: acp.ProtocolVersion,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:        false,
			PromptCapabilities: &acp.PromptCapabilities{},
			MCPCapabilities:    &acp.MCPCapabilities{HTTP: true},
		},
		AuthMethods: []acp.AuthMethod{{ID: "mock", Name: "Mock agent（无需凭据）"}},
	}, nil
}

func (a *agent) newSession(_ context.Context, raw json.RawMessage) (any, error) {
	var p acp.NewSessionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "参数不合法: " + err.Error()}
	}
	s := &session{id: newID(), cwd: p.Cwd}

	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()

	if a.stateDir != "" {
		if err := os.WriteFile(filepath.Join(a.stateDir, s.id), []byte(p.Cwd), 0o644); err != nil {
			return nil, fmt.Errorf("mock: 持久化会话失败: %v", err)
		}
	}
	log.Printf("mock: 新建会话 %s cwd=%s", s.id, p.Cwd)
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
		// 进程重启过：从落盘记录恢复，模拟 dsh 的 /var/lib/dsh 行为
		if cwd, err := os.ReadFile(filepath.Join(a.stateDir, p.SessionID)); err == nil {
			s = &session{id: p.SessionID, cwd: string(cwd)}
			a.sessions[p.SessionID] = s
			ok = true
		}
	}
	a.mu.Unlock()

	if !ok {
		return nil, &acp.RPCError{Code: -32602, Message: "mock: 未知会话 " + p.SessionID}
	}
	log.Printf("mock: 恢复会话 %s", s.id)
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
		return nil, &acp.RPCError{Code: -32602, Message: "mock: 未知会话 " + p.SessionID}
	}

	a.mu.Lock()
	cancel := s.cancel
	a.mu.Unlock()
	if cancel != nil {
		log.Printf("mock: 收到取消 %s", p.SessionID)
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
	s, ok := a.sessions[p.SessionID]
	if ok && s.cancel != nil {
		a.mu.Unlock()
		// 一个 ACP 会话同时只能有一个进行中的 prompt，这是协议不变量。
		// 真 agent 可能排队，这里直接拒绝，好让上层的串行化逻辑被测出来。
		return nil, &acp.RPCError{Code: -32600, Message: "mock: 该会话已有进行中的 prompt"}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if ok {
		s.cancel = cancel
	}
	a.mu.Unlock()

	if !ok {
		cancel()
		return nil, &acp.RPCError{Code: -32602, Message: "mock: 未知会话 " + p.SessionID}
	}
	defer func() {
		a.mu.Lock()
		s.cancel = nil
		a.mu.Unlock()
		cancel()
	}()

	if a.fail {
		return nil, &acp.RPCError{Code: -32603, Message: "mock: 模拟失败（等价于 agent 侧没有模型凭据）"}
	}

	input := strings.TrimSpace(joinText(p.Prompt))
	log.Printf("mock: prompt %s: %q", p.SessionID, input)

	if err := a.update(s.id, map[string]any{
		"sessionUpdate": "agent_thought_chunk",
		"content":       acp.ContentBlock{Type: "text", Text: "先确认工作目录，再动手。"},
	}); err != nil {
		return nil, err
	}
	if !a.pause(runCtx) {
		return cancelled(), nil
	}

	// 工具调用：先发起，再向 client 请求权限。真实 agent 就是这个顺序。
	tcID := "tc-" + newID()[:8]
	if err := a.update(s.id, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    tcID,
		"title":         "写入 notes.md",
		"kind":          "edit",
		"status":        "pending",
		"rawInput":      json.RawMessage(`{"path":"notes.md"}`),
	}); err != nil {
		return nil, err
	}

	allowed := true
	if !a.noPerm {
		out, err := a.askPermission(runCtx, s.id, tcID)
		if err != nil {
			// 取消时这条请求会被掐断（client 那边回了拒绝后 runCtx 已取消），
			// 这不是故障，按「本轮被取消」收尾。真 agent 也是这么做的：
			// 把 cancel 当正常控制流，而不是当成要上报的错误。
			if runCtx.Err() != nil {
				return cancelled(), nil
			}
			return nil, err
		}
		log.Printf("mock: 权限结果 %+v", out.Outcome)
		allowed = out.Outcome.Outcome == acp.OutcomeSelected && strings.HasPrefix(out.Outcome.OptionID, "allow")
	}
	if !allowed {
		_ = a.update(s.id, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    tcID,
			"status":        "failed",
			"rawOutput":     json.RawMessage(`{"error":"权限被拒绝"}`),
		})
		return acp.PromptResult{StopReason: "refusal"}, nil
	}

	if a.write {
		if err := os.WriteFile(filepath.Join(s.cwd, "notes.md"), []byte(input+"\n"), 0o644); err != nil {
			log.Printf("mock: 写 notes.md 失败: %v", err)
		}
	}
	_ = a.update(s.id, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    tcID,
		"status":        "completed",
		"rawOutput":     json.RawMessage(`{"bytes":` + strconv.Itoa(len(input)) + `}`),
	})

	reply := "已处理：" + input
	runes := []rune(reply)
	for i := 0; i < len(runes); i += a.chunk {
		end := i + a.chunk
		if end > len(runes) {
			end = len(runes)
		}
		if err := a.update(s.id, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       acp.ContentBlock{Type: "text", Text: string(runes[i:end])},
		}); err != nil {
			return nil, err
		}
		if !a.pause(runCtx) {
			return cancelled(), nil
		}
	}

	log.Printf("mock: prompt 完成 %s", p.SessionID)
	return acp.PromptResult{StopReason: "end_turn"}, nil
}

// askPermission 反向调用 client 等审批。这是 ACP 与「事后审计」的根本区别：
// agent 真的会阻塞在这里，所以 client 不回就等于任务卡死。
func (a *agent) askPermission(ctx context.Context, sid, tcID string) (acp.RequestPermissionResult, error) {
	return acp.CallAs[acp.RequestPermissionResult](ctx, a.conn, "session/request_permission",
		acp.RequestPermissionParams{
			SessionID: sid,
			ToolCall: acp.ToolCall{
				SessionUpdate: "tool_call",
				ToolCallID:    tcID,
				Title:         "写入 notes.md",
				Kind:          "edit",
			},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Name: "允许一次", Kind: acp.KindAllowOnce},
				{OptionID: "allow-always", Name: "总是允许", Kind: acp.KindAllowAlways},
				{OptionID: "reject-once", Name: "拒绝", Kind: acp.KindRejectOnce},
			},
		})
}

func (a *agent) update(sid string, body map[string]any) error {
	return a.conn.Notify("session/update", map[string]any{
		"sessionId": sid,
		"update":    body,
	})
}

// pause 等待一个间隔；被取消时返回 false。
func (a *agent) pause(ctx context.Context) bool {
	if a.delay <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(a.delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func cancelled() acp.PromptResult { return acp.PromptResult{StopReason: "cancelled"} }

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
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
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
