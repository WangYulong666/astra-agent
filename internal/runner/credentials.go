package runner

import (
	"context"
	"time"
)

// SessionScope 签发凭据时能看到的会话信息。
//
// 为什么不直接传 store.Session：签发发生在会话落库**之前**（沙箱要先起来才能拿到
// workspace 和 agentSessionId），此时 Session 还是半成品。用一个只含必要字段的类型，
// 既避免调用方伪造一堆用不上的字段，也避免实现方误以为这些字段可信。
type SessionScope struct {
	SessionID string
	TenantID  string
	UserID    string
}

// CredentialIssuer 给会话签发沙箱内可用的模型凭据。
//
// 存在的意义：一期把真 key 直接转发进了沙箱进程的环境变量，agent 执行一句 env
// 就能读到，模型自己也能把 key 泄漏出去——这与设计文档「LLM key 不进沙箱」直接冲突。
// 有了这个接口，沙箱里放的是短期虚拟 key，真 key 只留在控制面。
//
// 返回的 env 会合并进 AgentSpec.Env（同名覆盖），expiresAt 只用于日志与后续轮换判断。
type CredentialIssuer interface {
	IssueForSession(ctx context.Context, scope SessionScope) (env map[string]string, expiresAt time.Time, err error)
}
