package runner

import (
	"github.com/WangYulong666/astra-agent/internal/acp"
)

// PermissionPolicy 决定「agent 请求某个操作时给不给」。
//
// M1 用预设而不是人工审批：审批流转是 M3 的事，但**决策必须留痕**——
// 每次请求都会落一条 approval_req 事件（含 policy 名与决策），
// 这样 M3 把「自动决策」换成「等人点」时，历史数据不用迁移。
type PermissionPolicy struct {
	// Name 写进事件，便于事后区分是哪条策略放行的。
	Name string
	// Allow 允许的 tool kind。ACP 的 kind 取值：
	// read / edit / delete / move / search / execute / think / fetch / other
	Allow map[string]bool

	// Interactive 表示「白名单之外的一律挂起等人」。为 true 时 Decide 只处理白名单，
	// 剩下的交给 runner 的审批注册表——而不是在这里猜一个默认值。
	Interactive bool
}

// AutoAllowed 判断某个 kind 是否可以不问人直接放行。
//
// 注意零值语义：Interactive 为 false 时没有「自动放行」的概念，统一走 Decide；
// 这样 allow-all / deny-all 的行为不会被这个新字段悄悄改掉。
func (p PermissionPolicy) AutoAllowed(kind string) bool {
	return p.Allow != nil && p.Allow[kind]
}

// WorkspaceWrite 是 M1 的默认预设：允许在工作目录内读写与执行，拒绝外网抓取与删除。
//
// 为什么不是「全放行」：`fetch` 是数据外流通道，`delete` 在沙箱里也可能删掉挂载进来的东西。
// 为什么不是「全拒绝」：那样 agent 什么也干不了，演示不出流式与工具调用。
func WorkspaceWrite() PermissionPolicy {
	return PermissionPolicy{
		Name: "workspace-write",
		Allow: map[string]bool{
			"read": true, "edit": true, "move": true,
			"search": true, "execute": true, "think": true, "other": true,
		},
	}
}

// Ask 是交互预设：读类操作直接放行，其余（执行、写文件、删除、联网）挂起等人点。
//
// 这是 M3 审批流的最小实现，但它不是「演示用的假审批」——调用真的阻塞在
// ACP 的 request_permission 上，agent 真的在等，拒绝后 agent 真的收不到结果。
// 默认放行 read/think/search 的理由：它们不改变沙箱状态，也不外流数据，
// 逐条问只会把审批面板刷满，反而让人习惯性点「允许」。
func Ask() PermissionPolicy {
	return PermissionPolicy{
		Name:        "ask",
		Interactive: true,
		Allow: map[string]bool{
			"read": true, "think": true, "search": true,
		},
	}
}

// AllowAll 仅用于测试。生产配置里不该出现。
func AllowAll() PermissionPolicy {
	return PermissionPolicy{Name: "allow-all"}
}

// DenyAll 用于验证「拒绝后 agent 会收到什么」。
func DenyAll() PermissionPolicy {
	return PermissionPolicy{Name: "deny-all", Allow: map[string]bool{}}
}

// Decide 选出要回复给 agent 的选项。
//
// 优先 allow_once 而不是 allow_always：allow_always 会让 agent 之后同类操作不再询问，
// 等于把审批能力一次性交出去。M3 做真实审批时这条更重要。
func (p PermissionPolicy) Decide(req acp.RequestPermissionParams) (acp.PermissionOption, bool) {
	kind := req.ToolCall.Kind
	// 注意默认是「拒绝」：Allow 为 nil 不等于全放行，否则零值策略会静默变成 allow-all。
	allowed := p.Name == "allow-all" || (p.Allow != nil && p.Allow[kind])
	if allowed {
		if opt, ok := req.Select(acp.KindAllowOnce); ok {
			return opt, true
		}
		if opt, ok := req.Select(acp.KindAllowAlways); ok {
			return opt, true
		}
	}
	if opt, ok := req.Select(acp.KindRejectOnce); ok {
		return opt, false
	}
	if opt, ok := req.Select(acp.KindRejectAlways); ok {
		return opt, false
	}
	return acp.PermissionOption{}, false
}

// PolicyByName 按名字取策略，供 CLI/服务端 flag 使用。
func PolicyByName(name string) PermissionPolicy {
	switch name {
	case "ask":
		return Ask()
	case "allow-all":
		return AllowAll()
	case "deny-all":
		return DenyAll()
	default:
		return WorkspaceWrite()
	}
}
