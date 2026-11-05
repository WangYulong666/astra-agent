// Package acp 实现 Agent Client Protocol v1 的客户端侧。
//
// 协议事实（2026-10 核实，非推测）：
//
//   - JSON-RPC 2.0，**NDJSON 分帧**：每行一条 JSON。不是 LSP 的 Content-Length 头。
//   - 双向请求：client 调 agent（initialize / session/*），
//     agent 也会调 client（session/request_permission、fs/read_text_file、fs/write_text_file、terminal/*）。
//   - 通知（session/update）没有 id，不期待响应。
//   - 版本锁 1：v2 仍是 experimental，不在本项目考虑范围。
//
// 这个包不关心传输。任何 io.ReadWriteCloser 都能用——本地 stdio（测试）和
// WSS（沙箱内 acp-proxy）走同一套代码。这是 M0 能在没有 KVM 的机器上验证的原因。
package acp

import (
	"encoding/json"
	"strconv"
)

// ProtocolVersion 是唯一支持的 ACP 版本。
const ProtocolVersion = 1

// ── JSON-RPC 信封 ─────────────────────────────────────────────

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError 是 JSON-RPC 错误体。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "acp: nil rpc error"
	}
	return "acp: rpc error " + strconv.Itoa(e.Code) + ": " + e.Message
}

// ── initialize ────────────────────────────────────────────────

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

type ClientCapabilities struct {
	FS       *FSCapabilities `json:"fs,omitempty"`
	Terminal bool            `json:"terminal,omitempty"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
}

type AgentCapabilities struct {
	LoadSession        bool                `json:"loadSession,omitempty"`
	PromptCapabilities *PromptCapabilities `json:"promptCapabilities,omitempty"`
	MCPCapabilities    *MCPCapabilities    `json:"mcpCapabilities,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ── session/new ───────────────────────────────────────────────

type NewSessionParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// MCPServer 是 session/new 里可挂载的 MCP 服务。
// 用接口 + 两个具体类型表达 stdio / http 两种形态。
type MCPServer interface {
	isMCPServer()
}

// MCPStdio 以子进程方式挂载 MCP server。
type MCPStdio struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []EnvVar `json:"env,omitempty"`
}

func (MCPStdio) isMCPServer() {}

// MCPHTTP 以 streamable HTTP 方式挂载 MCP server。
// 注意：dsh-acp 只声明支持 http，不支持 sse。
type MCPHTTP struct {
	Name    string       `json:"name"`
	URL     string       `json:"url"`
	Headers []HTTPHeader `json:"headers,omitempty"`
}

func (MCPHTTP) isMCPServer() {}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ── session/prompt ────────────────────────────────────────────

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// ContentBlock 是 prompt 的内容块。当前只用到 text；
// 注意 dsh 的 promptCapabilities.image / audio / embeddedContext 全为 false，
// 端侧传图只能落盘让 agent 用工具读，不能走原生 image block。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func Text(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// ── session/update（通知，agent → client）──────────────────────

// SessionUpdate 是通知信封。Update 里还有一层 type 字段，见 Update 系列类型。
type SessionUpdate struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// UpdateKind 便于在不解完整结构的情况下先看类型。
type UpdateKind struct {
	Type string `json:"sessionUpdate"` // 规范里字段名是 sessionUpdate
}

// AgentMessageChunk 是模型输出的增量文本。
type AgentMessageChunk struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// ToolCall 是工具调用开始。
type ToolCall struct {
	SessionUpdate string          `json:"sessionUpdate"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"` // execute / edit / delete / read / fetch / think / other
	Status        string          `json:"status,omitempty"`
	RawInput      json.RawMessage `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
	// Content 是工具产出的结构化内容（ACP 的 content[]：文本片段、diff 等）。
	// 保留原始形态不解析：不同 agent 的 content 形状差异很大，
	// 在协议层强行归一化只会丢掉我们还没意识到的字段。
	Content json.RawMessage `json:"content,omitempty"`
}

// ToolCallUpdate 是工具调用的状态更新。
type ToolCallUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	ToolCallID    string          `json:"toolCallId"`
	Status        string          `json:"status,omitempty"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
	Content       json.RawMessage `json:"content,omitempty"`
}

// ── session/request_permission（agent → client 的请求）─────────

type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCall           `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

// 权限结论
const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

// 权限选项类型
const (
	KindAllowOnce    = "allow_once"
	KindAllowAlways  = "allow_always"
	KindRejectOnce   = "reject_once"
	KindRejectAlways = "reject_always"
)

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// Select 按 kind 选出第一个匹配的选项；找不到返回 false。
func (p RequestPermissionParams) Select(kind string) (PermissionOption, bool) {
	for _, o := range p.Options {
		if o.Kind == kind {
			return o, true
		}
	}
	return PermissionOption{}, false
}

// ── session/resume / session/close ────────────────────────────

type ResumeSessionParams struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd,omitempty"`
}

type CloseSessionParams struct {
	SessionID string `json:"sessionId"`
}
