package distill

// 事件流 → 步骤。
//
// 一个工具调用在事件流里是「一条 tool_call_start + N 条 tool_call_update」，
// 用 toolCallId 归并成一步。start 里带入参，update 里带终态，两者都可能缺：
// 有的 agent 只发一条 tool_call 且直接带 completed，有的在 run 被取消时
// 永远等不到 update。所以合并规则是「后来的非空字段覆盖前面的」，
// 而不是「只认 start」或「只认最后一条 update」。

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// Step.Outcome 的取值。空字符串表示这一步没有拿到终态（run 中途被取消等）。
const (
	OutcomeOK     = "ok"
	OutcomeFailed = "failed"
	OutcomeDenied = "denied"
)

// minOKSteps 是允许沉淀的最低成功步数。
//
// 为什么是 2 而不是 1：一步的「技能」等价于一句命令，没有复用价值；
// 两步才构成「先做什么再做什么」的顺序，那才是技能。
const minOKSteps = 2

// eventPayload 是 tool_call_start / tool_call_update / approval_* 的公共形状。
// 用宽松结构而不是逐类型定义：不同 agent 在 rawInput 里塞什么字段我们管不了，
// 但 toolCallId / status 这两个是 ACP 规定的，可以依赖。
type eventPayload struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Decision   string          `json:"decision"`
	Allowed    *bool           `json:"allowed"`
}

type call struct {
	title  string
	kind   string
	input  json.RawMessage
	status string
	// denied / failed 是「见过明确否定证据」的粘性标记：
	// 后续 update 说 completed 也不能把一次拒绝洗成成功。
	denied bool
	failed bool
}

// replaySteps 把事件流还原成步骤。
//
// 返回：截断后的步骤、工具调用总数、被省略的步数。
// 截断只影响产出，不影响统计——正文里的「另有 N 步」要的是真实总数。
func replaySteps(events []store.Event, maxSteps int) (steps []Step, total int, omitted int) {
	var order []*call
	byID := map[string]*call{}

	get := func(id string) *call {
		if id != "" {
			if c, ok := byID[id]; ok {
				return c
			}
		}
		c := &call{}
		if id != "" {
			byID[id] = c
		}
		order = append(order, c)
		return c
	}

	for _, ev := range events {
		switch ev.Type {
		case store.EventToolStart:
			var p eventPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			c := get(p.ToolCallID)
			mergeCall(c, p)
		case store.EventToolUpdate:
			var p eventPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			c := get(p.ToolCallID)
			mergeCall(c, p)
		case store.EventApprovalResult:
			var p eventPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			// 审批结论是「没被允许」的硬证据：agent 那边通常还会补一条
			// tool_call_update(failed)，但有的直接结束 run，只有这条能查。
			if isDeny(p.Decision) {
				if p.ToolCallID != "" {
					get(p.ToolCallID).denied = true
				}
			}
		case store.EventApprovalReq:
			var p eventPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			if p.Allowed != nil && !*p.Allowed && p.ToolCallID != "" {
				get(p.ToolCallID).denied = true
			}
		}
	}

	total = len(order)
	if maxSteps > 0 && total > maxSteps {
		omitted = total - maxSteps
		order = order[:maxSteps]
	}
	steps = make([]Step, 0, len(order))
	for i, c := range order {
		steps = append(steps, buildStep(i+1, c))
	}
	return steps, total, omitted
}

func mergeCall(c *call, p eventPayload) {
	if p.Title != "" {
		c.title = p.Title
	}
	if p.Kind != "" {
		c.kind = p.Kind
	}
	if len(p.RawInput) > 0 && string(p.RawInput) != "null" {
		c.input = p.RawInput
	}
	if p.Status != "" {
		c.status = strings.ToLower(p.Status)
	}
	if p.Allowed != nil && !*p.Allowed {
		c.denied = true
	}
	switch strings.ToLower(p.Status) {
	case "failed", "error":
		c.failed = true
	}
}

func isDeny(decision string) bool {
	switch strings.ToLower(decision) {
	case "deny", "denied", "reject", "rejected", "timeout", "cancelled", "canceled":
		return true
	}
	return false
}

// outcomeOf 给出这一步的结论。denied 优先于 failed：被拒绝的操作根本没执行，
// 说它「失败」会让人以为是命令自己报错。
func outcomeOf(c *call) string {
	if c.denied {
		return OutcomeDenied
	}
	if c.failed {
		return OutcomeFailed
	}
	switch c.status {
	case "completed", "complete", "success", "succeeded", "ok", "done":
		return OutcomeOK
	case "failed", "error":
		return OutcomeFailed
	case "cancelled", "canceled", "rejected", "denied":
		return OutcomeDenied
	}
	// pending / in_progress / 空：没有终态证据，不给 ok 也不给 failed。
	return ""
}

func buildStep(index int, c *call) Step {
	tool := toolName(c.title, c.kind, c.input)
	val := keyArg(tool, c.input)
	if val == "" {
		val = c.title
	}
	val = singleLine(val)
	return Step{
		Index:   index,
		Tool:    tool,
		Command: truncateRunes(val, 300),
		Summary: truncateRunes(val, 120),
		Outcome: outcomeOf(c),
	}
}

func countOK(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.Outcome == OutcomeOK {
			n++
		}
	}
	return n
}

// ── 工具名与关键参数 ─────────────────────────────────────────

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,31}$`)

// kindTool 把 ACP 的 kind 映射成工具名。kind 是协议枚举，值域比 title 稳定。
var kindTool = map[string]string{
	"execute": "bash",
	"exec":    "bash",
	"shell":   "bash",
	"edit":    "write",
	"write":   "write",
	"read":    "read",
	"delete":  "delete",
	"fetch":   "fetch",
	"search":  "search",
	"think":   "think",
}

// toolName 归一化出工具名。
//
// 三个来源按可靠性排序：
//  1. 入参里的 tool / toolName / name——最明确，agent 自己声明的；
//  2. title，且只在它长得像个标识符时（"bash"、"Read"）采用。
//     title 经常是「给人看的一句话」（"rm -rf /tmp/x"），拿它当工具名会把
//     命令重复一遍，还会在步骤行里出现真正的命令藏在一句话里；
//  3. kind 映射（execute → bash 等）。
func toolName(title, kind string, input json.RawMessage) string {
	if m := decodeObject(input); m != nil {
		for _, k := range []string{"tool", "toolName", "tool_name", "name"} {
			if s, ok := m[k].(string); ok {
				s = strings.ToLower(strings.TrimSpace(s))
				if identRe.MatchString(s) {
					return s
				}
			}
		}
	}
	if t := strings.ToLower(strings.TrimSpace(title)); identRe.MatchString(t) {
		return t
	}
	if t, ok := kindTool[strings.ToLower(strings.TrimSpace(kind))]; ok {
		return t
	}
	return "tool"
}

// keyArg 取这一步「最有信息量的一个字段」。
//
// 每个工具的入参里都有一项最像「这一步干了什么」，取它比取整个 JSON 更好读、
// 也更稳定（JSON 一多字段就会在渲染时被截断，关键信息反而看不见）。
// 其余工具退回「入参 JSON 前 120 字符」——宁可难看，也不要凭猜测编字段名。
func keyArg(tool string, input json.RawMessage) string {
	m := decodeObject(input)
	if m == nil {
		if s := rawString(input); s != "" {
			return s
		}
		return ""
	}
	switch tool {
	case "bash", "shell", "exec", "terminal", "execute":
		if s := strField(m, "command", "cmd", "script"); s != "" {
			return s
		}
	case "write", "edit", "multiedit", "patch", "notebookedit":
		if s := strField(m, "file_path", "filePath", "path"); s != "" {
			return s
		}
	case "read", "view", "cat":
		if s := strField(m, "file_path", "filePath", "path"); s != "" {
			return s
		}
	}
	// 通用兜底：常见字段优先，再不行才是整段 JSON。
	if s := strField(m, "command", "file_path", "filePath", "path", "url", "query", "pattern"); s != "" {
		return s
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return truncateRunes(string(b), 120)
}

func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// rawString 处理「入参本身就是一个字符串」的情况（有的 agent 直接发 shell 命令串）。
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}
