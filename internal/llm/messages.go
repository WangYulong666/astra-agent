package llm

// Anthropic Messages 协议支持（设计文档 D6 / T1）。
//
// 为什么是「按协议族分流」而不是「OpenAI ↔ Messages 格式互转」：转换要覆盖
// tool_use / tool_result / thinking 这些结构差异，一旦转换有损，模型表现为
// 「变笨」却没有报错，排查成本远高于各协议各解析一遍。
//
// 所以本文件只做两件事：
//  1. 从路径判断这次请求属于哪个协议族（detectProtocol）
//  2. 把 Messages 的 usage 事件合并成一次记账（messagesUsage）
// 请求体与响应体本身原样透传。

import (
	"encoding/json"
	"strings"
)

// Protocol 一次请求的协议族。决定 usage 怎么解析、请求体能不能被整形。
type Protocol int

const (
	// ProtocolTransparent 未识别的路径（/models、/v1/files 等）：只转发，不记账。
	// 放在零值：拿不准时宁可不解析，也不要把看不懂的 body 当 usage 记一条错账。
	ProtocolTransparent Protocol = iota
	// ProtocolOpenAI /v1/chat/completions。
	ProtocolOpenAI
	// ProtocolMessages Anthropic Messages，/v1/messages 及其子路径。
	ProtocolMessages
)

// String 便于日志与测试输出。
func (p Protocol) String() string {
	switch p {
	case ProtocolOpenAI:
		return "openai"
	case ProtocolMessages:
		return "messages"
	default:
		return "transparent"
	}
}

// detectProtocol 按路径判定协议族。
//
// 入参可以是网关收到的原始路径（/v1/llm/anthropic/v1/messages）、剥掉前缀后的
// 上游路径（/anthropic/v1/messages），也可以带 query 与尾部斜杠——判定只看
// 路径分段，所以三种形态结论一致，调用方不必先做归一化。
func detectProtocol(path string) Protocol {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i] // query 里出现 messages 之类的字样不该影响判定
	}
	path = strings.TrimRight(strings.ToLower(path), "/")
	if path == "" {
		return ProtocolTransparent
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasSuffix(path, "/chat/completions") {
		return ProtocolOpenAI
	}
	// 为什么按「有 messages 这一段」而不是「以 /messages 结尾」：
	// /messages/count_tokens、/messages/batches 属于同一协议族（同一套事件与
	// 同一套 usage 语义），只看结尾会把它们误判成透明转发，等于丢账。
	for _, seg := range strings.Split(path, "/") {
		if seg == "messages" {
			return ProtocolMessages
		}
	}
	return ProtocolTransparent
}

// ── Messages usage 解析 ───────────────────────────────────────
//
// 事件形状（流式）：
//   event: message_start
//   data: {"type":"message_start","message":{"model":"...","usage":{"input_tokens":9,"output_tokens":1}}}
//   event: content_block_delta
//   data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
//   event: message_delta
//   data: {"type":"message_delta","usage":{"output_tokens":42}}
//   event: message_stop
//   data: {"type":"message_stop"}
//
// 关键差异（与 OpenAI 不同，照抄 OpenAI 的解析必然记成 0）：
//   - usage 分散在首帧与尾帧两处，必须合并成一次记账；
//   - message_delta 的 output_tokens 是**累计值**，直接覆盖而非累加；
//   - input_tokens 只出现在 message_start，尾帧没有。

// messagesUsageJSON 只取需要计费的字段。
//
// 用指针而不是 int64：要区分「字段没出现」与「字段是 0」。
// message_delta 里没有 input_tokens，若用零值会让首帧的 input 被清掉。
type messagesUsageJSON struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// messagesEvent 覆盖两种 usage 载体：message_start 的 message.usage 与
// message_delta 的顶层 usage。其余字段（content block、stop_reason）不解析——
// 网关只搬字节，不需要理解内容。
type messagesEvent struct {
	Type  string `json:"type"`
	Model string `json:"model"`
	// Message 仅 message_start（非流式响应则是响应体顶层字段）有。
	Message *struct {
		Model string             `json:"model"`
		Usage *messagesUsageJSON `json:"usage"`
	} `json:"message"`
	Usage *messagesUsageJSON `json:"usage"`
}

// messagesUsage 流式 Messages 的 usage 累积器。
//
// 一次性把首帧与尾帧收进来，等流结束再回调一次：网关的记账语义是「一次调用
// 一条账」，中途回调会让同一次调用在库里出现两条半截记录。
type messagesUsage struct {
	in, out         int64
	haveIn, haveOut bool
	model           string
}

func (t *messagesUsage) feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return // event: 行、空行、注释心跳
	}
	var ev messagesEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		// 上游偶尔插非 JSON 帧，解不出就当没看见，绝不因此中断转发。
		return
	}

	if m := ev.Message; m != nil {
		if m.Model != "" {
			t.model = m.Model
		}
		if u := m.Usage; u != nil {
			if u.InputTokens != nil {
				t.in, t.haveIn = *u.InputTokens, true
			}
			// message_start 的 output_tokens 是「已经开始生成」的占位值（通常是 1），
			// 不是最终值。这里先记下当下限：万一上游中途断流、没有 message_delta，
			// 至少不会把已经生成的那部分记成 0。最终值由 message_delta 覆盖。
			if u.OutputTokens != nil {
				t.out, t.haveOut = *u.OutputTokens, true
			}
		}
	}

	if u := ev.Usage; u != nil {
		// message_delta 给的 output_tokens 是累计值，覆盖而不是累加。
		// 累加会把 1+42 记成 43（首帧占位值也被算进去），长回答累积误差更大。
		if u.OutputTokens != nil {
			t.out, t.haveOut = *u.OutputTokens, true
		}
		// input 以 message_start 为准；只有上游压根没发 message_start 时才兜底取这里的。
		if u.InputTokens != nil && !t.haveIn {
			t.in, t.haveIn = *u.InputTokens, true
		}
	}

	if ev.Model != "" {
		t.model = ev.Model
	}
}

func (t *messagesUsage) modelName() string { return t.model }

func (t *messagesUsage) result() (int64, int64, bool) {
	// 一帧 usage 都没见过就不记账：记一条 0/0 的账等于给账单塞噪音，
	// 而且会掩盖「上游改协议了」这件事。
	if !t.haveIn && !t.haveOut {
		return 0, 0, false
	}
	return t.in, t.out, true
}

// usageFromMessagesBody 非流式 Messages 响应的 usage：
// {"type":"message","model":"...","usage":{"input_tokens":9,"output_tokens":42}}。
func usageFromMessagesBody(body []byte) (model string, in, out int64, ok bool) {
	var ev messagesEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", 0, 0, false
	}
	if ev.Usage == nil {
		return "", 0, 0, false
	}
	in, out, ok = 0, 0, true
	if ev.Usage.InputTokens != nil {
		in = *ev.Usage.InputTokens
	}
	if ev.Usage.OutputTokens != nil {
		out = *ev.Usage.OutputTokens
	}
	return ev.Model, in, out, ok
}
