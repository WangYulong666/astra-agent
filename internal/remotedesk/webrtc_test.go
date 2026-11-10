package remotedesk

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// ── 分片协议（纯函数） ──────────────────────────────────────────

// 1 片 / 多片 / 正好整除 都要能原样往返。
func TestChunkFrameRoundTrip(t *testing.T) {
	sizes := []int{1, 100, rtcChunkPayload, rtcChunkPayload + 1, 3*rtcChunkPayload + 7}
	for _, size := range sizes {
		frame := make([]byte, size)
		for i := range frame {
			frame[i] = byte(i*7 + 3)
		}
		chunks, err := chunkFrame(42, frame)
		if err != nil {
			t.Fatalf("size=%d chunkFrame: %v", size, err)
		}
		wantChunks := (size + rtcChunkPayload - 1) / rtcChunkPayload
		if len(chunks) != wantChunks {
			t.Fatalf("size=%d 应切成 %d 片，实际 %d 片", size, wantChunks, len(chunks))
		}

		var asm frameAssembler
		var got []byte
		for i, c := range chunks {
			if len(c) <= rtcChunkHeader {
				t.Fatalf("size=%d 第 %d 片没有载荷", size, i)
			}
			out, err := asm.push(c, time.Now())
			if err != nil {
				t.Fatalf("size=%d 第 %d 片被拒: %v", size, i, err)
			}
			if i < len(chunks)-1 && out != nil {
				t.Fatalf("size=%d 第 %d 片就提前拼完了", size, i)
			}
			if out != nil {
				got = out
			}
		}
		if !bytes.Equal(got, frame) {
			t.Fatalf("size=%d 往返后内容不一致（拿到 %d 字节）", size, len(got))
		}
	}
}

// 头里的帧序号/分片号/总片数必须能被解析回来。
func TestChunkHeaderEncoding(t *testing.T) {
	h := chunkHeader{Seq: 0x1234, Index: 2, Total: 5}
	b := encodeChunkHeader(h)
	got, ok := decodeChunkHeader(b[:])
	if !ok || got != h {
		t.Fatalf("头往返失败: %+v ok=%v", got, ok)
	}
	if _, ok := decodeChunkHeader([]byte{1, 2, 3}); ok {
		t.Fatal("不足 4 字节的头应被拒")
	}
	// 总片数为 0 / 分片号越界都是坏头
	if _, ok := decodeChunkHeader([]byte{0, 1, 0, 0}); ok {
		t.Fatal("总片数为 0 应被拒")
	}
	if _, ok := decodeChunkHeader([]byte{0, 1, 3, 3}); ok {
		t.Fatal("分片号 >= 总片数应被拒")
	}
}

// 乱序分片必须整帧作废，不能拼出错位数据。
func TestAssemblerRejectsOutOfOrder(t *testing.T) {
	frame := make([]byte, 3*rtcChunkPayload)
	chunks, err := chunkFrame(7, frame)
	if err != nil {
		t.Fatalf("chunkFrame: %v", err)
	}
	var asm frameAssembler
	if _, err := asm.push(chunks[0], time.Now()); err != nil {
		t.Fatalf("第一片: %v", err)
	}
	// 跳过第 2 片直接给第 3 片
	if _, err := asm.push(chunks[2], time.Now()); !errors.Is(err, errChunkOutOfOrder) {
		t.Fatalf("乱序应返回 errChunkOutOfOrder，实际 %v", err)
	}
	if asm.total != 0 {
		t.Fatal("乱序后应清空半成品")
	}
}

// 缺片超时：半帧等太久就丢掉，后续帧重新开始。
func TestAssemblerMissingChunkTimeout(t *testing.T) {
	frame := make([]byte, 3*rtcChunkPayload)
	chunks, err := chunkFrame(9, frame)
	if err != nil {
		t.Fatalf("chunkFrame: %v", err)
	}
	var asm frameAssembler
	start := time.Now()
	if _, err := asm.push(chunks[0], start); err != nil {
		t.Fatalf("第一片: %v", err)
	}
	// 超时后再来同一帧的第 2 片
	_, err = asm.push(chunks[1], start.Add(assembleTimeout+time.Millisecond))
	if !errors.Is(err, errChunkIncomplete) {
		t.Fatalf("缺片超时应返回 errChunkIncomplete，实际 %v", err)
	}
	if asm.drops == 0 {
		t.Fatal("超时应记一次丢弃")
	}

	// 超时之后从新的一帧完整重来仍能成功。
	var asm2 frameAssembler
	var out []byte
	for i, c := range chunks {
		got, err := asm2.push(c, start.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatalf("重来第 %d 片: %v", i, err)
		}
		if got != nil {
			out = got
		}
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("超时恢复后拼出来的帧不对")
	}
}

// 超过 255 片的帧直接拒绝（头里放不下总片数）。
func TestChunkFrameTooLarge(t *testing.T) {
	frame := make([]byte, rtcMaxChunks*rtcChunkPayload+1)
	if _, err := chunkFrame(1, frame); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("超大帧应返回 errFrameTooLarge，实际 %v", err)
	}
	if _, err := chunkFrame(1, nil); err == nil {
		t.Fatal("空帧应报错")
	}
}

// ── 解包 ───────────────────────────────────────────────────────

// manager 会用 trackedRuntime 包一层，裸断言在真实路径上必然失败。
func TestWebRTCForUnwrapsTrackedRuntime(t *testing.T) {
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), NewFakeCapture(1), NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	wrapped := &trackedRuntime{Runtime: rt}
	if _, ok := any(wrapped).(WebRTCProvider); ok {
		t.Fatal("trackedRuntime 不该直接满足 WebRTCProvider（这正是不用裸断言的原因）")
	}
	p, ok := WebRTCFor(wrapped)
	if !ok || p == nil {
		t.Fatal("WebRTCFor 应剥掉包装拿到 frameRuntime")
	}
	if p != rt.(WebRTCProvider) {
		t.Fatal("WebRTCFor 返回的 provider 不是底层那一个")
	}
	if _, ok := WebRTCFor(nil); ok {
		t.Fatal("nil Runtime 不该被认成 provider")
	}
	// 实现了 Runtime 但不实现 WebRTCProvider 的类型（这里用假 Runtime）也不该被认出来。
	if _, ok := WebRTCFor(plainRuntime{rt}); ok {
		t.Fatal("没实现 WebRTCProvider 的 Runtime 不该被认出来")
	}
}

// plainRuntime 只暴露 Runtime 的方法，用来验证「不支持 RTC 的运行时」这条分支。
type plainRuntime struct{ Runtime }

// ── Pion 互连（服务端角色 + 客户端角色，同进程） ────────────────

// TestWebRTCRoundTrip 是这一轮唯一能证明「DataChannel 真能跑起来」的用例：
// 两个 Pion PeerConnection 互连，服务端推一帧、客户端发一次点击。
//
// 它依赖本机 UDP 回环；拿不到端口时 Skip 并说明原因（不是静默通过）。
func TestWebRTCRoundTrip(t *testing.T) {
	api, err := rtcStack()
	if err != nil {
		t.Skipf("本机无法启用 WebRTC（%v），跳过互连用例", err)
	}

	pol := testPolicy()
	pol.MaxFPS = 20
	// 故意用满宽 + 高质量：产出的 JPEG 远大于单片载荷（16KB），
	// 这样这条用例真的走到「多片重组」而不是碰运气。
	pol.MaxWidth = 1920
	pol.Quality = 85
	in := NewFakeInput()
	rt, err := newRuntime(activeSession(ModeControl), pol, NewFakeCapture(16).SetSize(1920, 1080), in, nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	provider, ok := WebRTCFor(rt)
	if !ok {
		t.Fatal("newRuntime 返回的运行时必须实现 WebRTCProvider")
	}
	if !provider.RTCSupported() {
		t.Skip("RTCSupported=false，跳过互连用例")
	}

	client, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("客户端 PeerConnection: %v", err)
	}
	defer client.Close()

	// 客户端创建 input 通道（服务端在 OnDataChannel 里收）。
	ordered := true
	inputDC, err := client.CreateDataChannel(rtcLabelInput, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("创建 input 通道: %v", err)
	}
	inputOpen := make(chan struct{})
	var inputOnce sync.Once
	inputDC.OnOpen(func() { inputOnce.Do(func() { close(inputOpen) }) })

	// 客户端重组服务端推来的帧。
	frameCh := make(chan []byte, 8)
	var asm frameAssembler
	client.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != rtcLabelFrames {
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			frame, err := asm.push(m.Data, time.Now())
			if err != nil {
				return
			}
			if frame == nil {
				return
			}
			select {
			case frameCh <- frame:
			default:
			}
		})
	})

	offer := buildLocalSDP(t, client, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	answer, err := provider.Answer(ctx, offer)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !containsCandidate(answer) {
		t.Fatalf("answer 里没有 ICE 候选（非 trickle 必须自带）:\n%s", answer)
	}
	if err := client.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		t.Fatalf("客户端 SetRemoteDescription: %v", err)
	}

	select {
	case <-inputOpen:
	case <-time.After(15 * time.Second):
		t.Fatal("15 秒内 input DataChannel 没打开（ICE 没连通？）")
	}
	t.Log("DataChannel 已连通")

	select {
	case frame := <-frameCh:
		if len(frame) == 0 {
			t.Fatal("收到的帧是空的")
		}
		img, err := jpeg.Decode(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("frames 通道收到的不是合法 JPEG: %v（%d 字节）", err, len(frame))
		}
		if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
			t.Fatalf("帧尺寸非法: %v", img.Bounds())
		}
		if len(frame) <= rtcChunkPayload {
			t.Fatalf("这一帧只有 %d 字节（单片载荷 %d），没覆盖多片重组路径", len(frame), rtcChunkPayload)
		}
		t.Logf("frames 通道收到一帧 JPEG：%d 字节（%d 片），%d×%d",
			len(frame), (len(frame)+rtcChunkPayload-1)/rtcChunkPayload,
			img.Bounds().Dx(), img.Bounds().Dy())
	case <-time.After(15 * time.Second):
		t.Fatal("15 秒内没收到视频帧")
	}

	// 输入：走 input DC 的 JSON 必须落到 Runtime.Input（限速/mode 校验都复用）。
	if err := inputDC.SendText(`{"op":"click","x":0.5,"y":0.25}`); err != nil {
		t.Fatalf("发送 click: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if evs := in.Events(); len(evs) > 0 {
			if evs[0].Op != "click" || evs[0].X != 0.5 || evs[0].Y != 0.25 {
				t.Fatalf("注入事件不对: %+v", evs[0])
			}
			if rt.Stats().Inputs == 0 {
				t.Fatal("Stats.Inputs 没涨")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("10 秒内 input 通道的点击没有落到注入后端")
}

// buildLocalSDP 做出带候选的本地 SDP（客户端角色的 offer，或任意一端）。
//
// 非 trickle 的客户端必须自己把候选拼进 SDP：Pion 只在 OnICECandidate 里
// 吐候选，不会写回 LocalDescription。
func buildLocalSDP(t *testing.T, pc *webrtc.PeerConnection, offer bool) string {
	t.Helper()

	gathered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var cands []string
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			once.Do(func() { close(gathered) })
			return
		}
		if line := c.ToJSON().Candidate; line != "" {
			mu.Lock()
			cands = append(cands, line)
			mu.Unlock()
		}
	})

	var desc webrtc.SessionDescription
	var err error
	if offer {
		desc, err = pc.CreateOffer(nil)
	} else {
		desc, err = pc.CreateAnswer(nil)
	}
	if err != nil {
		t.Fatalf("CreateOffer/Answer: %v", err)
	}
	if err := pc.SetLocalDescription(desc); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内 ICE 候选没收齐")
	}
	local := pc.LocalDescription()
	if local == nil {
		t.Fatal("本地描述为空")
	}
	mu.Lock()
	defer mu.Unlock()
	return sdpWithCandidates(local.SDP, cands)
}

func containsCandidate(sdp string) bool {
	return bytes.Contains([]byte(sdp), []byte("a=candidate:"))
}

// 候选拼装：没有候选时原样返回；有候选且 SDP 里已经有 a=candidate 时不重复追加。
func TestSDPWithCandidates(t *testing.T) {
	base := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"
	if got := sdpWithCandidates(base, nil); got != base {
		t.Fatal("没有候选时应原样返回")
	}
	got := sdpWithCandidates(base, []string{"candidate:1 1 udp 1 10.0.0.2 5000 typ host"})
	if !containsCandidate(got) {
		t.Fatalf("候选没拼进去:\n%s", got)
	}
	again := sdpWithCandidates(got, []string{"candidate:2 1 udp 1 10.0.0.3 5001 typ host"})
	if again != got {
		t.Fatal("已经有候选时不该重复追加")
	}
}

// offer 里没有 data channel 段（客户端没建任何 DC）时要快速失败，
// 而不是等 10 秒 DC 打开超时。
func TestAnswerRejectsOfferWithoutDataChannel(t *testing.T) {
	rt, err := newRuntime(activeSession(ModeView), testPolicy(), NewFakeCapture(1), NewFakeInput(), nil, nil)
	if err != nil {
		t.Fatalf("newRuntime: %v", err)
	}
	defer rt.Close()

	offer := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\na=setup:actpass\r\n"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rt.(WebRTCProvider).Answer(ctx, offer); err == nil {
		t.Fatal("没有 data channel 段的 offer 应该报错")
	} else if !bytes.Contains([]byte(err.Error()), []byte("data channel")) {
		t.Fatalf("错误信息应指向 data channel，实际 %v", err)
	}
	if _, err := rt.(WebRTCProvider).Answer(ctx, "   "); err == nil {
		t.Fatal("空 offer 应该报错")
	}
}
