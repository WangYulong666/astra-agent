package remotedesk

// WebRTC 媒体通道（Pion v4）。
//
// 为什么是 DataChannel + 单帧 JPEG 而不是视频轨：纯 Go 没有可用的 H.264/VP8
// 编码器（VideoToolbox 是另一个 cgo 项目，不在本轮）。所以这里买到的不是带宽，
// 而是「P2P 直连少一跳 + SCTP 拥塞控制 + ICE 自动选路」。
//
// 三条硬约束：
//  1. **回退永远可用**：任何一步失败都只让 RTC 这条路不通，WS + JPEG 继续跑。
//     Answer 出错只回 rtc-failed，绝不断连、绝不停 WS 的 binary 帧。
//  2. **懒创建**：只有客户端发 offer 才建 PeerConnection。每条 WS 连接都建一个
//     PC 等于每条连接都开一套 ICE，纯浪费（绝大多数连接不会用 RTC）。
//  3. **绝不阻塞抓屏泵**：DC 积压超过 4MB 直接丢帧并计 Dropped，等着 SCTP
//     慢慢发只会让延迟越积越大（用户看到的是几十秒前的画面）。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	// rtcLabelFrames 是服务端创建、单向推画面的通道。
	rtcLabelFrames = "frames"
	// rtcLabelInput 是客户端创建、单向传输入的通道。
	rtcLabelInput = "input"

	// rtcMaxBufferedAmount 是背压阈值：DC 积压超过它就丢帧。
	rtcMaxBufferedAmount = 4 << 20
	// rtcChunkPayload 是单分片载荷上限。SCTP 单条消息的实际上限取决于对端
	// 的 max-message-size，16KB 是各端都能吃下的保守值。
	rtcChunkPayload = 16 << 10
	// rtcMaxChunks 是分片数上限：头部里分片号只有一个字节，255 片
	// （按 16KB/片算 ≈ 4MB）远超单帧 JPEG 的实际大小，超了宁可丢帧。
	rtcMaxChunks = 255
	// rtcChunkHeader 是自定义头长度：2 字节帧序号 + 1 字节分片号 + 1 字节总片数。
	rtcChunkHeader = 4

	// rtcGatherTimeout 是等 ICE 收集完成的上限（非 trickle，必须等齐）。
	rtcGatherTimeout = 5 * time.Second
	// rtcOpenTimeout 是等 frames 通道打开的上限。DC 打不开说明 ICE 没通，
	// 这时候把 RTC 这条路的 goroutine 收掉，WS 照样出图。
	rtcOpenTimeout = 10 * time.Second
	// rtcInputQueue 是输入队列长度。队列存在的意义：注入（osascript/cliclick）
	// 可能要几十毫秒，不能堵在 SCTP 的读循环里。
	rtcInputQueue = 64

	rtcICEDisconnected = 10 * time.Second // 默认 5s，放宽（手机切网/锁屏回来）
	rtcICEFailed       = 30 * time.Second
	rtcICEKeepAlive    = 2 * time.Second
)

// 分片相关的错误。都是「丢这一帧、继续」级别，不该影响连接。
var (
	errChunkShort      = errors.New("remotedesk: 分片长度不足")
	errChunkBadTotal   = errors.New("remotedesk: 分片头非法（总片数为 0）")
	errChunkOutOfOrder = errors.New("remotedesk: 分片乱序")
	errChunkIncomplete = errors.New("remotedesk: 上一帧缺片，已丢弃")
	errFrameTooLarge   = errors.New("remotedesk: 帧太大，分片数超过 255")
)

// ── 分片协议 ────────────────────────────────────────────────────
//
// 4 字节头 + 载荷：
//
//	0..1  uint16 BE  帧序号（每帧 +1，绕回 0）
//	2     uint8      分片号 0..Total-1
//	3     uint8      总片数 Total（1..255）
//
// 为什么要有总片数：SCTP 的「消息边界」只保证一条 DC 消息不被拆，不保证
// 「一帧的 N 条消息」同时到齐；没有 Total 就无法判断何时可以交给解码器。
// 为什么不把帧序号放 4 字节：帧序号只需要在「上一帧没收完」时区分新旧，
// 16 位足够（20fps 下 54 分钟才绕回一次）。

// chunkHeader 是一条分片的头。
type chunkHeader struct {
	Seq   uint16
	Index uint8
	Total uint8
}

// encodeChunkHeader 写 4 字节头（大端，跨端一致）。
func encodeChunkHeader(h chunkHeader) [rtcChunkHeader]byte {
	var b [rtcChunkHeader]byte
	binary.BigEndian.PutUint16(b[0:2], h.Seq)
	b[2] = h.Index
	b[3] = h.Total
	return b
}

// decodeChunkHeader 解析 4 字节头。
func decodeChunkHeader(b []byte) (chunkHeader, bool) {
	if len(b) < rtcChunkHeader {
		return chunkHeader{}, false
	}
	h := chunkHeader{
		Seq:   binary.BigEndian.Uint16(b[0:2]),
		Index: b[2],
		Total: b[3],
	}
	if h.Total == 0 || h.Index >= h.Total {
		return chunkHeader{}, false
	}
	return h, true
}

// chunkFrame 把一帧 JPEG 切成若干分片（每片自带 4 字节头）。
// 空帧、超 255 片都返回错误——调用方丢帧即可。
func chunkFrame(seq uint16, frame []byte) ([][]byte, error) {
	if len(frame) == 0 {
		return nil, errChunkShort
	}
	total := (len(frame) + rtcChunkPayload - 1) / rtcChunkPayload
	if total > rtcMaxChunks {
		return nil, errFrameTooLarge
	}
	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		start := i * rtcChunkPayload
		end := start + rtcChunkPayload
		if end > len(frame) {
			end = len(frame)
		}
		buf := make([]byte, rtcChunkHeader+end-start)
		hdr := encodeChunkHeader(chunkHeader{
			Seq:   seq,
			Index: uint8(i),
			Total: uint8(total),
		})
		copy(buf[:rtcChunkHeader], hdr[:])
		copy(buf[rtcChunkHeader:], frame[start:end])
		out = append(out, buf)
	}
	return out, nil
}

// assembleTimeout 是一帧没收完时的等待上限：超过就丢掉半成品。
// 可靠有序的 DC 上正常不会触发，但「客户端换网/切后台」时半帧会卡住，
// 不清掉的话后面每一帧都会被误判成乱序。
const assembleTimeout = 2 * time.Second

// frameAssembler 是客户端侧的重组器（服务端单测也用它验证往返）。
// 只处理一条 DC 上的顺序流：同一时刻只保留一帧的半成品。
type frameAssembler struct {
	seq     uint16
	total   int
	next    int
	buf     []byte
	started time.Time
	timeout time.Duration

	// frames 是成功重组的帧数，drops 是被丢弃的半成品数（诊断用）。
	frames uint64
	drops  uint64
}

func (a *frameAssembler) limit() time.Duration {
	if a.timeout > 0 {
		return a.timeout
	}
	return assembleTimeout
}

// reset 丢掉当前半成品。
func (a *frameAssembler) reset() {
	a.seq = 0
	a.total = 0
	a.next = 0
	a.buf = nil
	a.started = time.Time{}
}

// push 喂一条分片。返回 (完整帧, nil) 表示这一帧齐了；
// 返回 (nil, nil) 表示还没收完；返回 (nil, err) 表示这条分片被拒（半成品已清）。
func (a *frameAssembler) push(chunk []byte, now time.Time) ([]byte, error) {
	h, ok := decodeChunkHeader(chunk)
	if !ok {
		a.reset()
		if len(chunk) < rtcChunkHeader {
			return nil, errChunkShort
		}
		return nil, errChunkBadTotal
	}
	payload := chunk[rtcChunkHeader:]

	// 半成品超时：先判超时再判序号，否则「上一帧缺片」会表现成「乱序」。
	if a.total > 0 && !a.started.IsZero() && now.Sub(a.started) > a.limit() {
		a.reset()
		a.drops++
		return nil, errChunkIncomplete
	}

	switch {
	case a.total == 0:
		// 新的一帧
		a.seq = h.Seq
		a.total = int(h.Total)
		a.next = 0
		a.buf = make([]byte, 0, int(h.Total)*rtcChunkPayload)
		a.started = now
	case h.Seq != a.seq:
		// 上一帧没吃完就来了新帧：丢掉半成品，从这一帧重新开始。
		a.reset()
		a.drops++
		return nil, errChunkIncomplete
	}

	if int(h.Index) != a.next {
		// 乱序/重复：整帧作废。宁可丢一帧，也不要把错位的数据拼给解码器。
		a.reset()
		a.drops++
		return nil, errChunkOutOfOrder
	}
	if len(a.buf)+len(payload) > int(h.Total)*rtcChunkPayload {
		a.reset()
		a.drops++
		return nil, errChunkOutOfOrder
	}
	a.buf = append(a.buf, payload...)
	a.next++

	if a.next == a.total {
		frame := a.buf
		a.reset()
		a.frames++
		return frame, nil
	}
	return nil, nil
}

// ── Pion 栈 ────────────────────────────────────────────────────

var (
	rtcInitOnce sync.Once
	rtcPionAPI  *webrtc.API
	rtcInitErr  error
)

// rtcStack 懒初始化 Pion 的 API（进程内一份）。
//
// 这里**不**创建 PeerConnection：创建它就意味着占用一个 UDP 端口，
// 而 RTCSupported() 每条 WS 都要问一次。真正的连通性只有协商才知道。
func rtcStack() (*webrtc.API, error) {
	rtcInitOnce.Do(func() {
		conn, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			rtcInitErr = fmt.Errorf("UDP 端口不可用（ICE 不可能成功）: %w", err)
			return
		}
		_ = conn.Close()

		var se webrtc.SettingEngine
		// 手机切 Wi-Fi/蜂窝、锁屏回来都可能短暂丢包，5 秒判断开太急。
		se.SetICETimeouts(rtcICEDisconnected, rtcICEFailed, rtcICEKeepAlive)
		// 不用 STUN/TURN：外网可达靠 relay（见 docs/design/remote-desktop-v2.md），
		// 这里只跑 host candidates（局域网直连）。
		rtcPionAPI = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	})
	return rtcPionAPI, rtcInitErr
}

// rtcSession 是一路运行时的 WebRTC 通道。
type rtcSession struct {
	pc     *webrtc.PeerConnection
	frames *webrtc.DataChannel

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once

	// inputLog 是「输入被拒/队列满」这类日志的限流时间戳（Unix 秒）。
	inputLog atomic.Int64
}

func (s *rtcSession) close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		// 关 PC 会把两条 DC 一起关掉，不用逐个 Close。
		if s.pc != nil {
			_ = s.pc.Close()
		}
		s.wg.Wait()
	})
}

// ── WebRTCProvider 实现 ────────────────────────────────────────

var _ WebRTCProvider = (*frameRuntime)(nil)

// RTCSupported 表示「本机有能力跑 WebRTC」：Pion 栈可构造 + 能绑 UDP 端口。
// 它不保证 ICE 一定打得通（对称 NAT 且无 TURN 时打不通），打不通就回退 WS。
func (r *frameRuntime) RTCSupported() bool {
	if r.closed.Load() {
		return false
	}
	_, err := rtcStack()
	return err == nil
}

// Answer 接收客户端 offer，返回带候选的 answer（非 trickle：双方各自聚合候选）。
//
// 流程：SetRemoteDescription → 建 frames DC → CreateAnswer → SetLocalDescription
// → 等 OnICECandidate(nil)（5s 上限）→ 把候选补进 SDP 返回。
func (r *frameRuntime) Answer(ctx context.Context, offerSDP string) (string, error) {
	if r.closed.Load() {
		return "", fmt.Errorf("%w: 帧流已关闭", ErrDenied)
	}
	if strings.TrimSpace(offerSDP) == "" {
		return "", fmt.Errorf("remotedesk: offer SDP 为空")
	}
	api, err := rtcStack()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}

	// 同一 runtime 只保留一路 RTC：重复 offer（换标签页/刷新）先关旧的，
	// 否则旧 PC 的 goroutine 和 UDP 端口会一直挂着。
	r.rtcMu.Lock()
	defer r.rtcMu.Unlock()
	if r.rtc != nil {
		r.rtc.close()
		r.rtc = nil
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", fmt.Errorf("remotedesk: 创建 PeerConnection 失败: %w", err)
	}
	// 任何一步失败都要把 PC 关掉，否则 UDPLite/DTLS 的 goroutine 会漏。
	fail := func(err error) (string, error) {
		_ = pc.Close()
		return "", err
	}

	// 候选收集：nil 表示收集完成（非 trickle 的关键信号）。
	gatherDone := make(chan struct{})
	var gatherOnce sync.Once
	var candMu sync.Mutex
	var cands []string
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			gatherOnce.Do(func() { close(gatherDone) })
			return
		}
		if line := c.ToJSON().Candidate; line != "" {
			candMu.Lock()
			cands = append(cands, line)
			candMu.Unlock()
		}
	})

	// 客户端的 offer 里必须有 data channel 段（它要建 input 通道）。没有的话
	// answer 里加不进 m=application，frames 通道永远打不开——早点说清楚，
	// 别让前端等 10 秒超时。
	if !strings.Contains(offerSDP, "m=application") {
		return fail(errors.New("remotedesk: offer 里没有 data channel 段（m=application），无法承载画面"))
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		return fail(fmt.Errorf("remotedesk: 解析 offer 失败: %w", err))
	}

	// frames 通道必须由服务端创建，且要在 CreateAnswer **之前**——它要进 SDP。
	ordered := true
	fdc, err := pc.CreateDataChannel(rtcLabelFrames, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return fail(fmt.Errorf("remotedesk: 创建 frames 通道失败: %w", err))
	}
	opened := make(chan struct{})
	var openOnce sync.Once
	fdc.OnOpen(func() { openOnce.Do(func() { close(opened) }) })

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(fmt.Errorf("remotedesk: CreateAnswer 失败: %w", err))
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(fmt.Errorf("remotedesk: SetLocalDescription 失败: %w", err))
	}

	select {
	case <-gatherDone:
	case <-time.After(rtcGatherTimeout):
		// 收不全也继续：已收集到的候选仍然可用，局域网的 host 候选通常几十毫秒就齐。
		r.logf("WebRTC 候选收集 %s 未完成，用已收集的 %d 个候选继续", rtcGatherTimeout, len(cands))
	case <-ctx.Done():
		return fail(ctx.Err())
	}

	local := pc.LocalDescription()
	if local == nil {
		return fail(errors.New("remotedesk: 本地描述为空"))
	}
	candMu.Lock()
	sdp := sdpWithCandidates(local.SDP, cands)
	candMu.Unlock()

	// 会话上下文挂在运行时上：Runtime.Close 会连带关掉 RTC。
	sessCtx, cancel := context.WithCancel(r.ctx)
	sess := &rtcSession{pc: pc, frames: fdc, cancel: cancel}
	r.startRTCWorkers(sessCtx, sess, opened)

	r.rtc = sess
	return sdp, nil
}

// sdpWithCandidates 保证 answer 里带上了候选。
//
// Pion 只通过 OnICECandidate 逐个吐候选，不会把它们写回 LocalDescription；
// 非 trickle 就必须自己拼。已经有 a=candidate 行时原样返回（未来 Pion 改了
// 行为也不会重复追加）。
func sdpWithCandidates(sdp string, cands []string) string {
	if len(cands) == 0 || strings.Contains(sdp, "a=candidate:") {
		return sdp
	}
	var b strings.Builder
	b.WriteString(sdp)
	if !strings.HasSuffix(sdp, "\r\n") {
		b.WriteString("\r\n")
	}
	for _, c := range cands {
		b.WriteString("a=")
		b.WriteString(c)
		b.WriteString("\r\n")
	}
	return b.String()
}

// startRTCWorkers 起两条后台流水：推帧 与 收输入。
func (r *frameRuntime) startRTCWorkers(ctx context.Context, sess *rtcSession, opened <-chan struct{}) {
	r.startRTCFrames(ctx, sess, opened)
	r.startRTCInput(ctx, sess)
}

// startRTCFrames 把帧通道里的 JPEG 一帧一片地写进 frames DC。
func (r *frameRuntime) startRTCFrames(ctx context.Context, sess *rtcSession, opened <-chan struct{}) {
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()

		select {
		case <-opened:
		case <-ctx.Done():
			return
		case <-time.After(rtcOpenTimeout):
			// ICE 没打通就收工：WS 那条路还在出图，这里再挂着只是浪费。
			r.logf("WebRTC frames 通道 %s 内未打开（ICE 未连通？），本次不用 RTC 推帧", rtcOpenTimeout)
			return
		}

		frames, err := r.Frames(ctx)
		if err != nil {
			r.logf("WebRTC 订阅帧失败: %v", err)
			return
		}
		var seq uint16
		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				// 背压：积压超阈值就丢这一帧。绝不阻塞——阻塞会让抓屏泵跟着排队，
				// 表现为「延迟越积越大」，比丢帧难受得多。
				if sess.frames.BufferedAmount() > rtcMaxBufferedAmount {
					r.dropped.Add(1)
					continue
				}
				chunks, cerr := chunkFrame(seq, f.JPEG)
				if cerr != nil {
					r.dropped.Add(1)
					continue
				}
				sent := true
				for _, c := range chunks {
					if err := sess.frames.Send(c); err != nil {
						sent = false
						break
					}
				}
				if !sent {
					r.dropped.Add(1)
					continue
				}
				seq++
			}
		}
	}()
}

// startRTCInput 在 input DC 上收 JSON 输入，转手交给 Runtime.Input。
//
// 注意复用 Runtime.Input：限速、mode 校验、坐标换算全在那一处，这里再写一遍
// 迟早会两套行为。
func (r *frameRuntime) startRTCInput(ctx context.Context, sess *rtcSession) {
	queue := make(chan InputEvent, rtcInputQueue)

	// 注入是可能几十毫秒的阻塞操作，必须在自己的 goroutine 里排队做，
	// 否则会堵住 SCTP 的读循环（连带堵住 ACK 处理）。
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-queue:
				if err := r.Input(ev); err != nil {
					r.logThrottled(&sess.inputLog, "WebRTC 输入被拒: %v", err)
				}
			}
		}
	}()

	sess.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != rtcLabelInput {
			// 我们不认识的通道：直接关掉，别让它占着 SCTP 流。
			_ = dc.Close()
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			var ev InputEvent
			if err := json.Unmarshal(m.Data, &ev); err != nil {
				r.logThrottled(&sess.inputLog, "WebRTC 输入不是合法 JSON: %v", err)
				return
			}
			select {
			case queue <- ev:
			default:
				// 队列满说明注入严重积压；丢一条比拖住读循环好。
				r.logThrottled(&sess.inputLog, "WebRTC 输入队列已满，丢弃一条 %s", ev.Op)
			}
		})
	})
}

// logThrottled 同类日志每分钟最多一条：输入被拒、队列满在坏网络下会刷屏，
// 把真正有用的「抓屏失败」挤没了。
func (r *frameRuntime) logThrottled(last *atomic.Int64, format string, args ...any) {
	now := time.Now().Unix()
	for {
		prev := last.Load()
		if prev != 0 && now-prev < 60 {
			return
		}
		if last.CompareAndSwap(prev, now) {
			break
		}
	}
	r.logf(format, args...)
}

// closeRTC 关掉这一路的 WebRTC 通道（幂等）。
func (r *frameRuntime) closeRTC() {
	r.rtcMu.Lock()
	sess := r.rtc
	r.rtc = nil
	r.rtcMu.Unlock()
	if sess != nil {
		sess.close()
	}
}

// ── 解包 ────────────────────────────────────────────────────────

// WebRTCFor 取出运行时背后的 WebRTCProvider。
//
// 为什么需要它：manager 把每个 Runtime 包进 trackedRuntime（用接口字段嵌入），
// 接口字段只提升 Runtime 的方法，所以 `rt.(WebRTCProvider)` 在真实路径上
// **永远失败**。这里在同一包里把这层包装剥掉；api 层只认这个函数。
func WebRTCFor(rt Runtime) (WebRTCProvider, bool) {
	for depth := 0; depth < 4 && rt != nil; depth++ {
		if p, ok := rt.(WebRTCProvider); ok {
			return p, true
		}
		wrapped, ok := rt.(*trackedRuntime)
		if !ok {
			return nil, false
		}
		rt = wrapped.Runtime
	}
	return nil, false
}
