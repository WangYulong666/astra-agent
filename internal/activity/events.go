package activity

import (
	"encoding/json"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// eventPayload 覆盖各类事件真实 payload 的并集。字段以 runner 的写入为准：
//
//   - run_state        : {"status", "stopReason", "reason", "error"}
//   - message_delta    : {"text"}                       （sink.delta 合并后的块）
//   - tool_call_*      : {"toolCallId","title","kind","status","rawInput","rawOutput","content"}
//     （manager.dispatch 的 tool_call / tool_call_update）
//   - approval_req     : {"toolCallId","title","kind","options","policy",
//     "decision","allowed","pending","expiresAt"}
//     （interactive 时 pending:true，auto 时 pending:false）
//   - approval_result  : {"toolCallId","decision","by"}  （decision=allow/deny/timeout）
//   - error            : {"message"}（runner.finish）/ {"error","kind","raw"}（SSE 解析兜底）
//   - child_*          : {"runId" / "childRunId","title","status","summary","error"}
//
// 用宽松结构而不是逐类型定义：不同来源塞的字段我们管不了，但 ACP 规定的
// toolCallId / status，以及 sink 落的 text/status/pending 都能依赖。
type eventPayload struct {
	Status     string          `json:"status"`
	StopReason string          `json:"stopReason"`
	Text       string          `json:"text"`
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	RawInput   json.RawMessage `json:"rawInput"`
	Decision   string          `json:"decision"`
	Allowed    *bool           `json:"allowed"`
	Pending    *bool           `json:"pending"`
	By         string          `json:"by"`
	Message    string          `json:"message"`
	Error      string          `json:"error"`
	Raw        string          `json:"raw"`
	RunID      string          `json:"runId"`
	ChildRunID string          `json:"childRunId"`
	Summary    string          `json:"summary"`
	Reason     string          `json:"reason"`
}

// eventEntries 把事件流翻译成条目，返回条目以及「事件流里是否已出现过终态
// run_state」——后者决定要不要由 run 元数据补一条终态 KindState。
//
// 两类合并是刻意的：
//   - 连续的 message_delta 合成一条 KindMessage（sink 已经按 50ms/4KB 合并过，
//     但一次长回答仍会落成多个块；这里再按「连续」归并成一条，别一段话十几行）；
//   - approval_result 不另起一条，而是按 toolCallId(Ref) 找到先前 pending 的
//     approval_req 条目，把它的 status 从 blocked 改成 done / failed。这样「现在
//     卡在哪」只反映还没结论的审批。
//
// 未知的 event 类型（agent_log / thought_delta…）不进账本：用户视角不需要
// 原始日志碎片，留着只会把时间线冲散。
func eventEntries(evs []store.Event, runID, sessionID string) ([]Entry, bool) {
	var out []Entry
	// ref(toolCallId) -> 那条 blocked 审批条目在 out 里的下标。
	approvalIdx := map[string]int{}
	sawTerminal := false

	var msg *Entry // 正在累积的连续 message_delta
	flushMsg := func() {
		if msg != nil {
			out = append(out, *msg)
			msg = nil
		}
	}

	for _, ev := range evs {
		if ev.Type != store.EventMessageDelta {
			flushMsg()
		}
		var p eventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			// 坏 payload 不该让整条账本挂掉：当成一条只读得到 type 的事件。
			p = eventPayload{}
		}
		base := func(kind, title, detail, status, ref string) Entry {
			return Entry{
				Time: ev.CreatedAt, Kind: kind, Title: title, Detail: detail,
				Status: status, Ref: ref, RunID: runID, SessionID: sessionID,
			}
		}

		switch ev.Type {
		case store.EventState:
			st := strings.ToLower(strings.TrimSpace(p.Status))
			if store.IsTerminal(st) {
				sawTerminal = true
			}
			out = append(out, base(KindState, stateTitle(st), stateDetail(p), stateStatus(st), ""))

		case store.EventToolStart, store.EventToolUpdate:
			out = append(out, base(KindTool, toolTitle(p), toolDetail(p),
				statusFromString(p.Status, ""), p.ToolCallID))

		case store.EventApprovalReq:
			// 只有「挂起等人」的审批才是阻塞项；auto 决策（pending:false）不进账本。
			if p.Pending != nil && *p.Pending {
				ref := p.ToolCallID
				approvalIdx[ref] = len(out)
				out = append(out, base(KindApproval, approvalTitle(p), "等待人工审批", StatusBlocked, ref))
			}

		case store.EventApprovalResult:
			status := approvalResultStatus(p.Decision)
			detail := approvalResultDetail(p)
			if idx, ok := approvalIdx[p.ToolCallID]; ok && p.ToolCallID != "" {
				out[idx].Status = status
				out[idx].Detail = detail
			} else {
				// 没有配对的 pending req（异常流），也不静默丢掉结论。
				out = append(out, base(KindApproval, "人工审批", detail, status, p.ToolCallID))
			}

		case store.EventMessageDelta:
			if msg == nil {
				msg = &Entry{
					Time: ev.CreatedAt, Kind: KindMessage, Title: "Agent 回复",
					Status: StatusDone, RunID: runID, SessionID: sessionID,
				}
			}
			msg.Detail += p.Text

		case store.EventError:
			out = append(out, base(KindError, "错误", errorDetail(p), StatusFailed, ""))

		case store.EventChildSpawned, store.EventChildResult:
			status := statusFromString(p.Status, p.Error)
			if p.Status == "" && ev.Type == store.EventChildResult && p.Error == "" {
				status = StatusDone
			}
			out = append(out, base(KindChild, childTitle(p.Title),
				childDetail(p.Summary, p.Error, p.Status), status, childEventRef(p)))
		}
	}
	flushMsg()
	return out, sawTerminal
}

// stateStatus：契约写死 run_state 的 status 推导——terminal→done，
// waiting_approval→blocked，running→running；其余（queued 等）保守归到 running。
func stateStatus(status string) string {
	if store.IsTerminal(status) {
		return StatusDone
	}
	switch status {
	case store.RunWaitingApproval:
		return StatusBlocked
	case store.RunRunning:
		return StatusRunning
	default:
		return StatusRunning
	}
}

func stateDetail(p eventPayload) string {
	if s := strings.TrimSpace(p.StopReason); s != "" {
		return s
	}
	return ""
}

func toolTitle(p eventPayload) string {
	if s := strings.TrimSpace(p.Title); s != "" {
		return truncateRunes(s, 80)
	}
	if s := strings.TrimSpace(p.Kind); s != "" {
		return truncateRunes(s, 80)
	}
	return "工具调用"
}

func toolDetail(p eventPayload) string {
	return strings.ToLower(strings.TrimSpace(p.Status))
}

func approvalTitle(p eventPayload) string {
	if s := strings.TrimSpace(p.Title); s != "" {
		return truncateRunes("等待审批 · "+s, 80)
	}
	if s := strings.TrimSpace(p.Kind); s != "" {
		return truncateRunes("等待审批 · "+s, 80)
	}
	return "等待人工审批"
}

// approvalResultStatus：allow 才 done，deny / timeout / 未知一律 failed。
func approvalResultStatus(decision string) string {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "allow", "allowed":
		return StatusDone
	default:
		return StatusFailed
	}
}

func approvalResultDetail(p eventPayload) string {
	dec := strings.ToLower(strings.TrimSpace(p.Decision))
	var label string
	switch dec {
	case "allow", "allowed":
		label = "已允许"
	case "deny", "denied":
		label = "已拒绝"
	case "timeout":
		label = "审批超时"
	default:
		label = "审批：" + p.Decision
	}
	if p.By != "" {
		label += "（" + p.By + "）"
	}
	return label
}

func errorDetail(p eventPayload) string {
	for _, s := range []string{p.Message, p.Error, p.Kind, p.Raw} {
		if strings.TrimSpace(s) != "" {
			return truncateRunes(s, 300)
		}
	}
	return "运行过程中发生错误"
}

func childEventRef(p eventPayload) string {
	for _, s := range []string{p.ChildRunID, p.RunID, p.ToolCallID} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
