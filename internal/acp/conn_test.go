package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// duplex 把一对 io.Pipe 拼成双向通道，用来在进程内模拟 agent。
// 这样协议层的测试完全不依赖 dsh、不依赖 LLM、不依赖 API key。
type duplex struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (d duplex) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) Close() error {
	_ = d.r.CloseWithError(ErrClosed)
	_ = d.w.CloseWithError(ErrClosed)
	return nil
}

func newPipePair() (client, agent duplex) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	return duplex{r: a2cR, w: c2aW}, duplex{r: c2aR, w: a2cW}
}

func TestCallAndResponse(t *testing.T) {
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	defer c.Close()

	enc := json.NewEncoder(aSide)
	dec := json.NewDecoder(aSide)

	go func() {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if req.Method != "initialize" {
			t.Errorf("agent 收到的方法 = %q，期望 initialize", req.Method)
		}
		_ = enc.Encode(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`),
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	raw, err := c.Call(ctx, "initialize", map[string]int{"protocolVersion": 1})
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	var got struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if got.ProtocolVersion != 1 {
		t.Fatalf("protocolVersion = %d，期望 1", got.ProtocolVersion)
	}
}

func TestRPCErrorPropagates(t *testing.T) {
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	defer c.Close()

	enc := json.NewEncoder(aSide)
	dec := json.NewDecoder(aSide)

	go func() {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		_ = enc.Encode(response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: -32603, Message: "turn failed: no API key"},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := c.Call(ctx, "session/prompt", struct{}{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("错误类型不是 *RPCError: %T", err)
	}
	if rpcErr.Code != -32603 {
		t.Fatalf("错误码 = %d，期望 -32603", rpcErr.Code)
	}
}

// 通知必须严格保序——session/update 的顺序是语义的一部分，
// 乱序会让前端看到错乱的消息流。这条约束靠 readLoop 里同步分发保证。
func TestNotificationOrderIsPreserved(t *testing.T) {
	const n = 50

	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	cl := NewClient(c)
	defer c.Close()

	var mu sync.Mutex
	var order []int
	done := make(chan struct{}, n)

	cl.OnUpdate = func(u SessionUpdate) {
		var chunk AgentMessageChunk
		_ = json.Unmarshal(u.Update, &chunk)
		var idx int
		_, _ = fmt.Sscanf(chunk.Content.Text, "%d", &idx)
		mu.Lock()
		order = append(order, idx)
		mu.Unlock()
		done <- struct{}{}
	}

	enc := json.NewEncoder(aSide)
	go func() {
		for i := 0; i < n; i++ {
			params, _ := json.Marshal(SessionUpdate{
				SessionID: "s1",
				Update:    json.RawMessage(fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}`, i)),
			})
			_ = enc.Encode(notification{JSONRPC: "2.0", Method: "session/update", Params: params})
		}
	}()

	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("只收到 %d/%d 条通知", i, n)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for i, v := range order {
		if v != i {
			t.Fatalf("第 %d 条通知的序号是 %d，顺序被打乱", i, v)
		}
	}
}

// 双向请求：agent 主动向 client 要权限，client 回复后 agent 才能继续。
// 这是整套人工审批的协议基础——如果这条不通，平台就管不住 agent 的行为。
func TestPermissionRequestFromAgent(t *testing.T) {
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	cl := NewClient(c)
	defer c.Close()

	var (
		mu      sync.Mutex
		seen    *RequestPermissionParams
		permErr error
	)
	cl.OnPermission = func(_ context.Context, p RequestPermissionParams) (PermissionOutcome, error) {
		mu.Lock()
		seen = &p
		mu.Unlock()
		return PermissionOutcome{Outcome: OutcomeSelected, OptionID: "opt-allow-once"}, nil
	}

	enc := json.NewEncoder(aSide)
	dec := json.NewDecoder(aSide)

	permParams := `{
		"sessionId":"s1",
		"toolCall":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"rm -rf /tmp/x","kind":"execute"},
		"options":[
			{"optionId":"opt-allow-once","name":"允许一次","kind":"allow_once"},
			{"optionId":"opt-allow-always","name":"始终允许","kind":"allow_always"},
			{"optionId":"opt-reject","name":"拒绝","kind":"reject_once"}
		]
	}`

	go func() {
		_ = enc.Encode(request{
			JSONRPC: "2.0",
			ID:      json.RawMessage("42"),
			Method:  "session/request_permission",
			Params:  json.RawMessage(permParams),
		})
	}()

	var resp response
	readDone := make(chan error, 1)
	go func() { readDone <- dec.Decode(&resp) }()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("agent 端读响应失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent 没有收到权限响应（client 可能没回）")
	}

	mu.Lock()
	defer mu.Unlock()
	if permErr != nil {
		t.Fatalf("权限处理出错: %v", permErr)
	}
	if seen == nil {
		t.Fatal("OnPermission 没有被调用")
	}
	if seen.ToolCall.Kind != "execute" {
		t.Fatalf("toolCall.kind = %q，期望 execute", seen.ToolCall.Kind)
	}
	if len(seen.Options) != 3 {
		t.Fatalf("options 数量 = %d，期望 3", len(seen.Options))
	}
	if _, ok := seen.Select(KindAllowAlways); !ok {
		t.Fatal("没能识别出 allow_always 选项")
	}

	if resp.Error != nil {
		t.Fatalf("响应里带错误: %v", resp.Error)
	}
	var out RequestPermissionResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("解析权限响应失败: %v", err)
	}
	if out.Outcome.Outcome != OutcomeSelected || out.Outcome.OptionID != "opt-allow-once" {
		t.Fatalf("权限结论 = %+v，期望 selected/opt-allow-once", out.Outcome)
	}
}

// 没配 OnPermission 时必须默认拒绝，不能静默放行。
func TestPermissionDefaultsToDeny(t *testing.T) {
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	NewClient(c) // 故意不设 OnPermission
	defer c.Close()

	enc := json.NewEncoder(aSide)
	dec := json.NewDecoder(aSide)

	go func() {
		_ = enc.Encode(request{
			JSONRPC: "2.0",
			ID:      json.RawMessage("7"),
			Method:  "session/request_permission",
			Params: json.RawMessage(`{
				"sessionId":"s1",
				"toolCall":{"toolCallId":"t1","kind":"execute"},
				"options":[{"optionId":"a","name":"允许","kind":"allow_once"}]
			}`),
		})
	}()

	var resp response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	var out RequestPermissionResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Outcome.Outcome != OutcomeCancelled {
		t.Fatalf("未配置审批时的结论 = %q，期望 cancelled（安全默认）", out.Outcome.Outcome)
	}
}

func TestCallTimeout(t *testing.T) {
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	defer c.Close()

	// io.Pipe 是无缓冲的：agent 端必须先有人把请求读走，否则 client 的写会一直阻塞，
	// 测的就不是「等响应超时」而是「写阻塞」了。这里读走但**故意不回响应**。
	go func() {
		dec := json.NewDecoder(aSide)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.Call(ctx, "session/new", struct{}{})
	if err == nil {
		t.Fatal("期望超时错误，实际为 nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误 = %v，期望 DeadlineExceeded", err)
	}
}

func TestIDNormalization(t *testing.T) {
	// 有些实现会把 id 从 5 返成 "5"，这里要能匹配上
	cSide, aSide := newPipePair()
	c := NewConn(cSide)
	defer c.Close()

	enc := json.NewEncoder(aSide)
	dec := json.NewDecoder(aSide)

	go func() {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		// 故意用字符串形式的 id 回复
		_ = enc.Encode(response{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`"` + string(req.ID) + `"`),
			Result:  json.RawMessage(`{"ok":true}`),
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Call(ctx, "ping", nil); err != nil {
		t.Fatalf("字符串 id 响应未被匹配: %v", err)
	}
}
