package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrClosed 表示连接已关闭。
var ErrClosed = errors.New("acp: connection closed")

// Handler 处理对方发来的请求或通知。
//
//   - 入站请求（带 id）：返回的 value 会被序列化成 result；返回 error 会被序列化成 error 体。
//   - 入站通知（无 id）：返回值被忽略。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Conn 是一条 ACP 连接。传输由调用方提供（stdio 或 WSS 都行）。
//
// 读写模型：
//   - 单读 goroutine，串行解码 NDJSON。
//   - **通知同步分发**——session/update 的顺序是语义的一部分，乱序会让前端看到错乱的消息流。
//   - **请求异步分发**——request_permission 会阻塞到人工审批返回，不能卡住读循环。
type Conn struct {
	rwc     io.ReadWriteCloser
	enc     *json.Encoder
	dec     *json.Decoder
	writeMu sync.Mutex

	pending  sync.Map // idKey -> chan *response
	handlers sync.Map // method -> Handler

	nextID atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	errOnce   sync.Once
	closeErr  error
}

// NewConn 包装一个读写通道并启动读循环。调用方负责在用完后 Close。
func NewConn(rwc io.ReadWriteCloser) *Conn {
	c := &Conn{
		rwc:    rwc,
		enc:    json.NewEncoder(rwc), // Encode 自带换行，正好是 NDJSON
		dec:    json.NewDecoder(rwc),
		closed: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Handle 注册某个方法的处理器。同一个方法重复注册会覆盖。
func (c *Conn) Handle(method string, h Handler) {
	c.handlers.Store(method, h)
}

// Done 在连接关闭时关闭。
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Err 返回导致连接关闭的错误。
func (c *Conn) Err() error {
	c.errOnce.Do(func() {}) // 读屏障，保证能看到已写入的值
	return c.closeErr
}

// Call 发一个请求并等响应。
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	select {
	case <-c.closed:
		return nil, c.Err()
	default:
	}

	id := c.nextID.Add(1)
	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	ch := make(chan *response, 1)
	c.pending.Store(idKey(rawID), ch)
	defer c.pending.Delete(idKey(rawID))

	p, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	if err := c.write(request{JSONRPC: "2.0", ID: rawID, Method: method, Params: p}); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, c.Err()
	}
}

// CallAs 是 Call + 反序列化的泛型包装。
func CallAs[T any](ctx context.Context, c *Conn, method string, params any) (T, error) {
	var zero T
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// Notify 发一个通知（不需要响应）。
func (c *Conn) Notify(method string, params any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(notification{JSONRPC: "2.0", Method: method, Params: p})
}

// Close 关闭连接。
func (c *Conn) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

// ── 内部 ─────────────────────────────────────────────────────

func (c *Conn) readLoop() {
	for {
		var raw json.RawMessage
		if err := c.dec.Decode(&raw); err != nil {
			c.shutdown(err)
			return
		}

		// 先只看信封，不预设是哪种消息
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue // 畸形帧：丢弃，不中断整条连接
		}

		hasID := len(probe.ID) > 0 && string(probe.ID) != "null"

		switch {
		case probe.Method != "" && hasID:
			// 入站请求（agent → client，比如 request_permission）
			go c.serveRequest(probe.Method, probe.ID, probe.Params)
		case probe.Method != "":
			// 入站通知（agent → client，比如 session/update）——同步，保序
			c.serveNotification(probe.Method, probe.Params)
		case hasID:
			// 响应（agent → client，对我们某个请求的回复）
			c.deliver(probe.ID, raw)
		}
	}
}

func (c *Conn) serveRequest(method string, id json.RawMessage, params json.RawMessage) {
	res, err := c.invoke(method, params)
	c.respond(id, res, err)
}

func (c *Conn) serveNotification(method string, params json.RawMessage) {
	// 通知没有错误通道，处理失败只能丢弃。
	_, _ = c.invoke(method, params)
}

func (c *Conn) invoke(method string, params json.RawMessage) (any, error) {
	if h, ok := c.handlers.Load(method); ok {
		return h.(Handler)(context.Background(), params)
	}
	return nil, &RPCError{Code: -32601, Message: "method not found: " + method}
}

func (c *Conn) respond(id json.RawMessage, result any, err error) {
	resp := response{JSONRPC: "2.0", ID: id}
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			resp.Error = rpcErr
		} else {
			resp.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
	} else {
		b, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = &RPCError{Code: -32603, Message: "marshal result: " + mErr.Error()}
		} else {
			resp.Result = b
		}
	}
	_ = c.write(resp)
}

func (c *Conn) deliver(id json.RawMessage, raw json.RawMessage) {
	v, ok := c.pending.LoadAndDelete(idKey(id))
	if !ok {
		return // 超时或取消后迟到的响应，丢弃
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return
	}
	ch := v.(chan *response)
	select {
	case ch <- &resp:
	default:
	}
}

func (c *Conn) write(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return c.Err()
	default:
	}
	if err := c.enc.Encode(v); err != nil {
		c.shutdown(err)
		return err
	}
	return nil
}

func (c *Conn) shutdown(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.errOnce.Do(func() { c.closeErr = err })
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.rwc.Close()
	})
}

// idKey 把 JSON 的 id 归一化成字符串。
// 把 5 和 "5" 视为同一个 id——有些实现会改变 id 的 JSON 类型。
func idKey(id json.RawMessage) string {
	return strings.Trim(string(id), `"`)
}

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
