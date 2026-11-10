package api

// WebRTC 信令（rtc-offer / rtc-answer / rtc-failed）与 hello.rtc 的测试。
//
// 复用 remote_test.go 的脚手架（fakeRemote / newFakeRuntime / newRemoteTestServer），
// 只额外定义「实现了 WebRTCProvider」的假运行时，以及一个能换掉 Open 返回值的
// 包装 manager（fakeRemote.Open 的字段是具体类型，塞不进别的运行时）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/remotedesk"
	"github.com/coder/websocket"
)

// rtcRuntime 是「支持 WebRTC」的假运行时：匿名嵌入 *fakeRuntime 拿到
// Runtime 的五个方法，再加 WebRTCProvider 的两个。
type rtcRuntime struct {
	*fakeRuntime
	supported bool
	answer    string
	answerErr error
	lastOffer string
}

func (r *rtcRuntime) RTCSupported() bool { return r.supported }

func (r *rtcRuntime) Answer(_ context.Context, offerSDP string) (string, error) {
	r.lastOffer = offerSDP
	if r.answerErr != nil {
		return "", r.answerErr
	}
	return r.answer, nil
}

// rtcRemote 让 Open 返回任意 Runtime（fakeRemote 的 runtime 字段是具体类型）。
type rtcRemote struct {
	*fakeRemote
	rt remotedesk.Runtime
}

func (m *rtcRemote) Open(context.Context, string, string) (remotedesk.Runtime, error) {
	return m.rt, nil
}

func newRTCRemote(rt remotedesk.Runtime) *rtcRemote {
	return &rtcRemote{fakeRemote: newFakeRemote(), rt: rt}
}

// readText 读一条 text 帧并解析成 map（断言 type 字段用）。
func readText(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	typ, data := remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageText {
		t.Fatalf("应为 text 帧，实际 %v", typ)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("不是 JSON 对象: %v（%s）", err, data)
	}
	return v
}

// 不支持 WebRTC 的运行时：rtc-offer 只回 rtc-failed，连接必须还能继续用。
func TestRemoteRTCUnsupportedFallsBack(t *testing.T) {
	fr := newFakeRemote()
	rt := newFakeRuntime() // 不实现 WebRTCProvider
	fr.runtime = rt
	srv := newRemoteTestServer(t, fr)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/remote/sessions/sess_1/stream"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	hello := readText(t, ctx, conn)
	if hello["type"] != "hello" {
		t.Fatalf("第一帧应是 hello: %v", hello)
	}
	if rtc, _ := hello["rtc"].(bool); rtc {
		t.Fatal("不支持 WebRTC 的运行时，hello.rtc 必须是 false")
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"rtc-offer","sdp":"v=0"}`)); err != nil {
		t.Fatalf("发送 rtc-offer: %v", err)
	}
	msg := readText(t, ctx, conn)
	if msg["type"] != "rtc-failed" {
		t.Fatalf("应回 rtc-failed，实际 %v", msg)
	}
	if reason, _ := msg["reason"].(string); reason == "" {
		t.Fatalf("rtc-failed 没带原因: %v", msg)
	}

	// 连接仍然可用：ping → pong。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("发送 ping: %v", err)
	}
	if msg := readText(t, ctx, conn); msg["type"] != "pong" {
		t.Fatalf("rtc-failed 之后应仍能 ping/pong，实际 %v", msg)
	}

	// WS 的 binary 帧照常出图（回退路径没被破坏）。
	want := []byte{0xff, 0xd8, 0xaa, 0xbb}
	rt.frames <- remotedesk.Frame{JPEG: want, Width: 1280, Height: 720, At: time.Now()}
	typ, data := remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageBinary {
		t.Fatalf("应收到 binary 帧，实际 %v", typ)
	}
	if string(data) != string(want) {
		t.Fatalf("binary 帧内容不对：%v", data)
	}
}

// 支持 WebRTC 的运行时：hello.rtc=true，rtc-offer 回 rtc-answer，
// 且 answer 之后 binary 帧**继续发**（服务端不做切换）。
func TestRemoteRTCOfferAnswer(t *testing.T) {
	rt := &rtcRuntime{
		fakeRuntime: newFakeRuntime(),
		supported:   true,
		answer:      "v=0\r\na=candidate:1 1 udp 1 10.0.0.2 5000 typ host\r\n",
	}
	srv := newRemoteTestServer(t, newRTCRemote(rt))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/remote/sessions/sess_1/stream"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	hello := readText(t, ctx, conn)
	if rtc, _ := hello["rtc"].(bool); !rtc {
		t.Fatal("支持 WebRTC 的运行时，hello.rtc 必须是 true")
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"rtc-offer","sdp":"v=0 offer"}`)); err != nil {
		t.Fatalf("发送 rtc-offer: %v", err)
	}
	msg := readText(t, ctx, conn)
	if msg["type"] != "rtc-answer" {
		t.Fatalf("应回 rtc-answer，实际 %v", msg)
	}
	if sdp, _ := msg["sdp"].(string); sdp != rt.answer {
		t.Fatalf("answer 没透传：%q", sdp)
	}
	if rt.lastOffer != "v=0 offer" {
		t.Fatalf("offer 没传到运行时：%q", rt.lastOffer)
	}

	// answer 之后 binary 帧继续发——前端可以自己忽略。
	want := []byte{0xff, 0xd8, 0x01, 0x02}
	rt.frames <- remotedesk.Frame{JPEG: want, Width: 1280, Height: 720, At: time.Now()}
	typ, data := remoteWSRead(t, ctx, conn)
	if typ != websocket.MessageBinary || string(data) != string(want) {
		t.Fatalf("rtc-answer 之后应继续收到 binary 帧，实际 %v %v", typ, data)
	}
}

// 空 offer / 协商失败都只回 rtc-failed，不断连。
func TestRemoteRTCOfferErrors(t *testing.T) {
	cases := []struct {
		name   string
		sdp    string
		err    error
		reason string
	}{
		{name: "empty_offer", sdp: "", reason: "sdp"},
		{name: "answer_failed", sdp: "v=0", err: errors.New("ICE 收集失败"), reason: "ICE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := &rtcRuntime{fakeRuntime: newFakeRuntime(), supported: true, answerErr: c.err}
			srv := newRemoteTestServer(t, newRTCRemote(rt))

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/remote/sessions/sess_1/stream"
			conn, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				t.Fatalf("WS 握手失败: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "bye")

			readText(t, ctx, conn) // hello

			body, _ := json.Marshal(map[string]string{"type": "rtc-offer", "sdp": c.sdp})
			if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
				t.Fatalf("发送 rtc-offer: %v", err)
			}
			msg := readText(t, ctx, conn)
			if msg["type"] != "rtc-failed" {
				t.Fatalf("应回 rtc-failed，实际 %v", msg)
			}
			if reason, _ := msg["reason"].(string); !strings.Contains(reason, c.reason) {
				t.Fatalf("reason 应包含 %q，实际 %q", c.reason, reason)
			}

			// 连接还在。
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
				t.Fatalf("发送 ping: %v", err)
			}
			if msg := readText(t, ctx, conn); msg["type"] != "pong" {
				t.Fatalf("rtc-failed 之后应仍能 ping/pong，实际 %v", msg)
			}
		})
	}
}
