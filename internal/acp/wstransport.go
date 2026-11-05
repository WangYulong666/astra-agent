package acp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// wsFrameLimit 单帧上限，与 acp-proxy 保持一致。
const wsFrameLimit = 8 << 20

// WSConn 把 WebSocket 消息流适配成 io.ReadWriteCloser，供 Conn 使用。
//
// 消息与字节流的映射：
//
//	收：一条 WS 文本消息 = 一行 NDJSON（补上 '\n'）
//	发：按 '\n' 切分，一段 = 一条 WS 文本消息
//
// 为什么值得做这层适配：acp.Conn 只依赖 io.ReadWriteCloser，所以**同一份协议代码**
// 在本地用 stdio 测、在沙箱里用 WSS 跑，不需要两套实现。这是 M0 能在没有 KVM 的
// 机器上验证全链路的原因。
type WSConn struct {
	c   *websocket.Conn
	r   *io.PipeReader
	w   *io.PipeWriter
	ctx context.Context
}

// DialWS 连上 acp-proxy 的 /acp 端点。
// token 通过 Authorization: Bearer 传递。
func DialWS(ctx context.Context, url, token string) (*WSConn, error) {
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}

	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:      hdr,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(wsFrameLimit)

	pr, pw := io.Pipe()
	s := &WSConn{c: c, r: pr, w: pw, ctx: ctx}
	go s.readPump()
	return s, nil
}

func (s *WSConn) readPump() {
	defer func() { _ = s.w.Close() }()
	for {
		_, data, err := s.c.Read(s.ctx)
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		buf := make([]byte, len(data)+1)
		copy(buf, data)
		buf[len(data)] = '\n' // 还原成 NDJSON 供 json.Decoder 消费
		if _, err := s.w.Write(buf); err != nil {
			return
		}
	}
}

func (s *WSConn) Read(p []byte) (int, error) { return s.r.Read(p) }

// Write 按换行切分后逐段发送。json.Encoder 每次 Encode 都以 '\n' 结尾，
// 所以正常情况下一段就对应一条完整消息。
func (s *WSConn) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		var chunk []byte
		if i < 0 {
			chunk, p = p, nil
		} else {
			chunk, p = p[:i], p[i+1:]
		}
		if len(chunk) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		err := s.c.Write(ctx, websocket.MessageText, chunk)
		cancel()
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (s *WSConn) Close() error {
	_ = s.r.CloseWithError(ErrClosed)
	_ = s.w.CloseWithError(ErrClosed)
	return s.c.Close(websocket.StatusNormalClosure, "")
}
