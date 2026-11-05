package acp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Client 是 ACP 客户端的高层封装。
//
// 它注册了两个 agent → client 的入站方法：
//   - session/update             通知，必须实现（否则事件全丢）
//   - session/request_permission 请求，必须实现（否则 agent 会一直阻塞等审批）
type Client struct {
	conn *Conn
	caps InitializeResult

	// OnUpdate 收到 session/update 时回调。**在读循环里同步执行**，
	// 所以实现必须快（落库、推 channel），不要在这里做网络 IO。
	OnUpdate func(SessionUpdate)

	// OnPermission 决定权限结果。返回 error 会让 agent 收到错误。
	// 未设置时默认拒绝——安全默认，避免忘记配置就自动放行。
	OnPermission func(ctx context.Context, p RequestPermissionParams) (PermissionOutcome, error)
}

// NewClient 在已有连接上装好 client 侧的方法处理器。
func NewClient(conn *Conn) *Client {
	c := &Client{conn: conn}
	conn.Handle("session/update", c.handleUpdate)
	conn.Handle("session/request_permission", c.handlePermission)
	return c
}

// Conn 返回底层连接。
func (c *Client) Conn() *Conn { return c.conn }

// Capabilities 返回 initialize 的结果（调用 Initialize 之后才有值）。
func (c *Client) Capabilities() InitializeResult { return c.caps }

// Initialize 握手。会校验协议版本，不匹配直接报错而不是继续跑。
func (c *Client) Initialize(ctx context.Context, caps ClientCapabilities) (InitializeResult, error) {
	res, err := CallAs[InitializeResult](ctx, c.conn, "initialize", InitializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: caps,
	})
	if err != nil {
		return res, err
	}
	if res.ProtocolVersion != ProtocolVersion {
		return res, fmt.Errorf("acp: 协议版本不匹配 agent=%d 期望=%d", res.ProtocolVersion, ProtocolVersion)
	}
	c.caps = res
	return res, nil
}

// NewSession 新建会话。mcp 传 nil 表示不挂载 MCP 服务。
func (c *Client) NewSession(ctx context.Context, cwd string, mcp []MCPServer) (string, error) {
	if mcp == nil {
		mcp = []MCPServer{} // 必须序列化成 []，不能是 null
	}
	res, err := CallAs[NewSessionResult](ctx, c.conn, "session/new", NewSessionParams{
		Cwd:        cwd,
		MCPServers: mcp,
	})
	if err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", fmt.Errorf("acp: session/new 返回了空 sessionId")
	}
	return res.SessionID, nil
}

// Prompt 发一轮用户输入，阻塞到 agent 给出 stopReason。
// 期间的所有增量事件通过 OnUpdate 回调。
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks ...ContentBlock) (PromptResult, error) {
	if len(blocks) == 0 {
		return PromptResult{}, fmt.Errorf("acp: prompt 不能为空")
	}
	return CallAs[PromptResult](ctx, c.conn, "session/prompt", PromptParams{
		SessionID: sessionID,
		Prompt:    blocks,
	})
}

// Resume 让 agent 恢复一个已存在的会话的**执行状态**。
//
// 注意：dsh-acp 刻意不做 transcript 回放，历史由调用方自己的事件表重建。
// 这里能恢复的是「agent 还记不记得这轮对话的上下文」，不是「把消息重放给前端」。
func (c *Client) Resume(ctx context.Context, sessionID, cwd string) error {
	_, err := c.conn.Call(ctx, "session/resume", ResumeSessionParams{SessionID: sessionID, Cwd: cwd})
	return err
}

// Cancel 请求取消当前这轮。是通知，不等待响应。
func (c *Client) Cancel(sessionID string) error {
	return c.conn.Notify("session/cancel", map[string]string{"sessionId": sessionID})
}

// CloseSession 关闭会话。是通知。
func (c *Client) CloseSession(sessionID string) error {
	return c.conn.Notify("session/close", CloseSessionParams{SessionID: sessionID})
}

// ── 入站处理器 ───────────────────────────────────────────────

func (c *Client) handleUpdate(_ context.Context, params json.RawMessage) (any, error) {
	var u SessionUpdate
	if err := json.Unmarshal(params, &u); err != nil {
		return nil, &RPCError{Code: -32602, Message: "bad session/update params: " + err.Error()}
	}
	if c.OnUpdate != nil {
		c.OnUpdate(u)
	}
	return nil, nil
}

func (c *Client) handlePermission(ctx context.Context, params json.RawMessage) (any, error) {
	var p RequestPermissionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "bad request_permission params: " + err.Error()}
	}

	var outcome PermissionOutcome
	if c.OnPermission == nil {
		// 安全默认：没人审批就拒绝，绝不静默放行
		outcome = PermissionOutcome{Outcome: OutcomeCancelled}
	} else {
		o, err := c.OnPermission(ctx, p)
		if err != nil {
			return nil, err
		}
		outcome = o
	}
	return RequestPermissionResult{Outcome: outcome}, nil
}
