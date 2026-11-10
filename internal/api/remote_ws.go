package api

// 远程桌面的画面通道：单条 WebSocket，binary 帧是 JPEG，text 帧是控制消息。
//
// 并发模型（这一段是理解本文件的关键，也是这类代码最容易写崩的地方）：
//
//	读 goroutine   ── conn.Read ──▶ 处理 input/quality/ping/rtc-offer ──▶ textCh
//	写 goroutine   ── conn.Write ◀── textCh / frameCh / ticker（stats+ping）
//	主 goroutine   ── rt.Frames ◀── 抓屏泵 ──▶ frameCh（满了丢最旧）
//
// **只有一个 goroutine 碰 conn 的写路径**。coder/websocket 的 Write 虽然内部有锁，
// 但多写者会让「hello 先于画面帧」「stats 不插进一帧中间」这两条顺序约束失效，
// 而且 Ping 也会写控制帧——把 Ping 留在写 goroutine 里才真的只有一条写路径。
//
// 三条退出路径（客户端断开 / ctx 取消 / 帧通道关闭）都会走到 remoteStream 的
// defer：先 conn.Close 再 rt.Close，读 goroutine 因 ctx 取消退出，写 goroutine
// 同理，不留 goroutine。
//
// WebRTC 信令复用这条 WS（不走第二条连接）：客户端发 rtc-offer，这里回
// rtc-answer 或 rtc-failed。**收到 answer 之后 WS 的 binary 帧照发不误**——
// 客户端自己决定用哪条路的画面，服务端不做「切换」，因为一旦切了，
// WebRTC 半路挂掉就会黑屏，而回退路径必须永远可用。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/remotedesk"
	"github.com/coder/websocket"
)

const (
	// remoteWSReadLimit 客户端消息上限。输入事件就几百字节，
	// 64KB 是给「虚拟键盘一次贴一整段文本」留的余量。
	remoteWSReadLimit = 64 << 10

	// remoteWSStatsEvery 既是 stats 间隔也是 ping 间隔。
	// 15 秒远小于常见反代 60 秒的空闲超时，静止画面下链路也不会被掐。
	remoteWSStatsEvery = 15 * time.Second
	// remoteWSPingTimeout ping 等 pong 的上限，超时即判死。
	remoteWSPingTimeout = 30 * time.Second
	// remoteWSWriteTimeout 单次写上限。没有它，一个不读数据的客户端
	// 能让写 goroutine 永远卡在 Write 上（goroutine + 内存一起漏）。
	remoteWSWriteTimeout = 30 * time.Second

	// remoteWSTextQueue 控制消息队列。控制消息不该丢（pong/error 丢了
	// 前端会以为服务端没反应），所以给一个有界缓冲 + 阻塞入队。
	remoteWSTextQueue = 16

	// remoteWSRTCTimeout 是服务端处理一次 offer 的上限。里面最多包含
	// 「等 5s ICE 收集」，20s 是给慢机器留的余量。
	remoteWSRTCTimeout = 20 * time.Second
)

// remoteWSHello 连上后的第一帧。
type remoteWSHello struct {
	Type      string           `json:"type"`
	SessionID string           `json:"sessionId"`
	Mode      string           `json:"mode"`
	Stats     remotedesk.Stats `json:"stats"`
	// RTC 表示「本会话可以试着协商 WebRTC」。前端据此决定要不要发 offer；
	// 它只是能力声明，真正的连通性由 rtc-answer 是否回复决定。
	RTC bool `json:"rtc"`
}

// remoteWSStats 周期水位帧。Stats 内联，前端直接读 fps/width/height。
type remoteWSStats struct {
	Type string `json:"type"`
	remotedesk.Stats
}

// remoteWSError 单条消息的处理失败。**只回这一帧，不断连**：
// 手滑打快了（限速）、按了不支持的键，都不该被踢下线。
type remoteWSError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// remoteWSRTCAnswer 协商成功：answer 里已经带上服务端收集好的 ICE 候选
// （非 trickle，客户端 SetRemoteDescription 之后就能直接连通）。
type remoteWSRTCAnswer struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// remoteWSRTCFailed 协商失败。**不断连**：WS + JPEG 那条路继续跑，
// 前端拿到这一帧就该退回「用 binary 帧渲染」。
type remoteWSRTCFailed struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// remoteOriginPatterns 决定哪些 Origin 允许连画面流。
//
// Accept 自己永远放行同源（Origin 的 host == 请求 Host），这里补的是
// 「手机经反代访问、页面域名与后端域名不同」的自部署场景。
// 默认只加本机回环（局域网直连 192.168.x.x:7788 的页面就是它自己），
// 其它域名必须由部署方显式写进 ASTRA_REMOTE_ORIGINS（逗号分隔）。
//
// 绝不写 "*"：那等于关掉 CSRF 防线，任何网页都能拿用户浏览器去连他的电脑。
func remoteOriginPatterns() []string {
	patterns := []string{
		"localhost", "localhost:*",
		"127.0.0.1", "127.0.0.1:*",
		"[::1]", "[::1]:*",
	}
	for _, p := range strings.Split(os.Getenv("ASTRA_REMOTE_ORIGINS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

// isWebSocketUpgrade 判断这是不是一次 WS 升级请求。
//
// 为什么要自己判：websocket.Accept 在握手失败时写的是 http.Error（text/plain），
// 而这条路由的其它错误都是 JSON。契约测试与前端探活都用普通 GET，
// 拿回一段纯文本会当场解析失败——所以非升级请求必须在 Accept 之前就分流。
func isWebSocketUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}

func (h *handler) remoteStream(w http.ResponseWriter, r *http.Request) {
	if !h.remoteReady(w) {
		return
	}
	if !isWebSocketUpgrade(r) {
		fail(w, http.StatusBadRequest,
			"需要 WebSocket 升级（浏览器用 new WebSocket，URL 带 ?ticket= 一次性票据）")
		return
	}

	p := h.principal(r)
	sessionID := r.PathValue("id")

	// 先取会话：hello 帧要带 mode，前端靠它决定是否显示「接管」控件。
	// 顺带把「不存在」在升级之前就变成 404——升级之后再报错只能发 error 帧。
	sess, err := h.cfg.Remote.GetSession(r.Context(), p.TenantID, sessionID)
	if err != nil {
		h.failRemote(w, err)
		return
	}

	rt, err := h.cfg.Remote.Open(r.Context(), p.TenantID, sessionID)
	if err != nil {
		// 待批准/已结束 → 403，不存在 → 404。
		h.failRemote(w, err)
		return
	}
	// 退订在返回时做（幂等）。会话本身不在这里结束——那是 EndSession 的事。
	defer func() { _ = rt.Close() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: remoteOriginPatterns(),
		// 明确写出来：不因为「手机连不上」就放开 Origin 校验。
		InsecureSkipVerify: false,
		// JPEG 已经压过一遍，再让 permessage-deflate 压一次只烧 CPU。
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// 走到这里说明握手在 Accept 内部失败，响应已由它写过（纯文本）。
		h.log.Printf("远程桌面 WS 升级失败: %v", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	conn.SetReadLimit(remoteWSReadLimit)

	h.serveRemoteStream(conn, rt, sess.ID, sess.Mode)
}

// serveRemoteStream 是升级之后的主循环：起两个 goroutine（读 / 写），
// 自己盯帧通道，任一方向结束就返回。
func (h *handler) serveRemoteStream(conn *websocket.Conn, rt remotedesk.Runtime, sessionID, mode string) {
	// Accept 之后请求的 context 不再随连接取消（连接已被 hijack），
	// 用独立的 ctx，由读/写任一侧出错时取消。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames, err := rt.Frames(ctx)
	if err != nil {
		// 已经 101 了，回不了 JSON；发一帧 error 再关，比静默消失好排查。
		b, _ := json.Marshal(remoteWSError{Type: "error", Message: err.Error()})
		_ = remoteWSWrite(ctx, conn, websocket.MessageText, b)
		return
	}

	textCh := make(chan []byte, remoteWSTextQueue)
	frameCh := make(chan remotedesk.Frame, 1)

	// hello 必须抢在画面帧之前：此刻 frameCh 还是空的，写 goroutine 拿到它一定在先。
	pushRemoteText(ctx, textCh, remoteWSHello{
		Type:      "hello",
		SessionID: sessionID,
		Mode:      mode,
		Stats:     rt.Stats(),
		RTC:       rtcAvailable(rt),
	})

	go h.remoteWriteLoop(ctx, cancel, conn, rt, textCh, frameCh)
	go h.remoteReadLoop(ctx, cancel, conn, rt, textCh)

	h.pumpRemoteFrames(ctx, cancel, frames, frameCh)
}

// pumpRemoteFrames 把订阅到的帧搬进写队列。慢客户端只会「跳帧」，绝不排队
// （排队的结果是延迟越积越大，用户看到的是几分钟前的画面）。
func (h *handler) pumpRemoteFrames(ctx context.Context, cancel context.CancelFunc,
	frames <-chan remotedesk.Frame, frameCh chan remotedesk.Frame) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				// 帧通道关闭 = 会话结束或抓屏泵不可恢复失败。
				return
			}
			select {
			case frameCh <- f:
			default:
				select {
				case <-frameCh: // 丢最旧的一帧
				default:
				}
				select {
				case frameCh <- f:
				default:
				}
			}
		}
	}
}

// remoteWriteLoop 是**唯一**的写者。所有写操作（text/binary/ping）都在这里，
// 于是「串行」是结构保证的，不依赖任何锁。
func (h *handler) remoteWriteLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn,
	rt remotedesk.Runtime, textCh <-chan []byte, frameCh <-chan remotedesk.Frame) {
	defer cancel()

	ticker := time.NewTicker(remoteWSStatsEvery)
	defer ticker.Stop()

	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case b, ok := <-textCh:
			if !ok {
				return
			}
			err = remoteWSWrite(ctx, conn, websocket.MessageText, b)
		case f, ok := <-frameCh:
			if !ok {
				return
			}
			err = remoteWSWrite(ctx, conn, websocket.MessageBinary, f.JPEG)
		case <-ticker.C:
			if b, merr := json.Marshal(remoteWSStats{Type: "stats", Stats: rt.Stats()}); merr == nil {
				if err = remoteWSWrite(ctx, conn, websocket.MessageText, b); err != nil {
					break
				}
			}
			// Ping 也是写：留在同一个 goroutine 里，别单开一个"保活协程"。
			pctx, pcancel := context.WithTimeout(ctx, remoteWSPingTimeout)
			err = conn.Ping(pctx)
			pcancel()
		}
		if err != nil {
			return
		}
	}
}

func (h *handler) remoteReadLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn,
	rt remotedesk.Runtime, textCh chan<- []byte) {
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// 客户端只发 text；来个 binary 就是协议用错了，忽略即可（不值得断连）。
		if typ != websocket.MessageText {
			continue
		}
		h.handleRemoteMessage(ctx, rt, textCh, data)
	}
}

// rtcAvailable 判断这一路能不能协商 WebRTC。
//
// 注意用 remotedesk.WebRTCFor 而不是直接的 `rt.(remotedesk.WebRTCProvider)`：
// manager 会返回 trackedRuntime 包装（用 Runtime 接口字段嵌入），接口字段只
// 提升 Runtime 的方法，直接断言在真实路径上永远失败。
func rtcAvailable(rt remotedesk.Runtime) bool {
	p, ok := remotedesk.WebRTCFor(rt)
	return ok && p.RTCSupported()
}

// handleRemoteMessage 处理一条客户端消息。任何失败都只回 error 帧。
func (h *handler) handleRemoteMessage(ctx context.Context, rt remotedesk.Runtime,
	textCh chan<- []byte, data []byte) {
	// 内嵌 InputEvent：协议是扁平的（{"type":"input","op":"move","x":..}），
	// 嵌成子对象会让客户端多套一层，前端手势代码已经按扁平写好了。
	var msg struct {
		Type string `json:"type"`
		remotedesk.InputEvent
		MaxWidth int    `json:"maxWidth"`
		FPS      int    `json:"fps"`
		Quality  int    `json:"quality"`
		SDP      string `json:"sdp"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		pushRemoteText(ctx, textCh, remoteWSError{Type: "error", Message: "消息不是合法 JSON: " + err.Error()})
		return
	}

	switch msg.Type {
	case "input":
		if err := rt.Input(msg.InputEvent); err != nil {
			// 限速/未授权/本机没权限：回一帧说明原因，连接留着。
			pushRemoteText(ctx, textCh, remoteWSError{Type: "error", Message: err.Error()})
		}
	case "quality":
		if err := rt.SetQuality(remotedesk.Quality{
			MaxWidth: msg.MaxWidth,
			FPS:      msg.FPS,
			Quality:  msg.Quality,
		}); err != nil {
			pushRemoteText(ctx, textCh, remoteWSError{Type: "error", Message: err.Error()})
		}
	case "rtc-offer":
		h.handleRTCOffer(ctx, rt, textCh, msg.SDP)
	case "rtc-answer":
		// 客户端把自己收到的 answer 回显回来（有的前端拿它做确认）。服务端
		// 不需要它，也不回任何东西：binary 帧照发，画面来源由客户端自己选。
	case "ping":
		pushRemoteText(ctx, textCh, map[string]string{"type": "pong"})
	default:
		pushRemoteText(ctx, textCh, remoteWSError{
			Type:    "error",
			Message: "未知消息类型: " + msg.Type + "（支持 input / quality / ping / rtc-offer）",
		})
	}
}

// handleRTCOffer 处理一次 offer。
//
// 这里是**同步**的（最长约 5s 等 ICE 收集完），代价是读循环在此期间不处理
// 后续消息。接受这个代价是因为：协商是一次性动作，而异步化会引入「同时两个
// offer 争同一个 PC」「answer 顺序错乱」这些状态机问题；写路径依旧只有写
// goroutine，不受影响。
func (h *handler) handleRTCOffer(ctx context.Context, rt remotedesk.Runtime,
	textCh chan<- []byte, offer string) {
	provider, ok := remotedesk.WebRTCFor(rt)
	if !ok || !provider.RTCSupported() {
		// 不支持就明确告诉前端，让它安心用 binary 帧；连接必须留着。
		pushRemoteText(ctx, textCh, remoteWSRTCFailed{
			Type:   "rtc-failed",
			Reason: "本机/本会话不支持 WebRTC（Pion 栈或 UDP 端口不可用）",
		})
		return
	}
	if strings.TrimSpace(offer) == "" {
		pushRemoteText(ctx, textCh, remoteWSRTCFailed{Type: "rtc-failed", Reason: "offer 缺少 sdp"})
		return
	}

	actx, cancel := context.WithTimeout(ctx, remoteWSRTCTimeout)
	defer cancel()
	answer, err := provider.Answer(actx, offer)
	if err != nil {
		// 协商失败只回 rtc-failed：WS + JPEG 那条路继续可用，画面不断。
		h.log.Printf("远程桌面 WebRTC 协商失败: %v", err)
		pushRemoteText(ctx, textCh, remoteWSRTCFailed{Type: "rtc-failed", Reason: err.Error()})
		return
	}
	pushRemoteText(ctx, textCh, remoteWSRTCAnswer{Type: "rtc-answer", SDP: answer})
}

func remoteWSWrite(ctx context.Context, conn *websocket.Conn, typ websocket.MessageType, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, remoteWSWriteTimeout)
	defer cancel()
	return conn.Write(wctx, typ, data)
}

// pushRemoteText 入队一条控制消息。队列满时阻塞（控制消息丢不得），
// 但 ctx 一取消就放手——写 goroutine 退出时第一件事就是 cancel。
func pushRemoteText(ctx context.Context, ch chan<- []byte, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case ch <- b:
	case <-ctx.Done():
	}
}
