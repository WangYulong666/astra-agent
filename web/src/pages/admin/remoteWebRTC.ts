// WebRTC 媒体通道（浏览器侧）：建 PC → 非 trickle 协商 → 接 frames DataChannel。
//
// 这份文件对应的服务端实现在 internal/remotedesk/webrtc.go，两边有三个**必须**
// 对齐的点，改动任何一侧都要同步：
//
// 1. **分片头只有 4 字节**，不是 6 字节。真实布局（服务端 encodeChunkHeader）：
//
//	0..1  uint16 大端  帧序号 seq（每帧 +1，绕回 0）
//	2     uint8        分片号 index（0..total-1）
//	3     uint8        总片数 total（1..255）
//	4..   载荷         该帧的一部分字节
//
// 任务书里写的「[2 字节帧序号][2 字节分片序号] + [2 字节分片总数]」是猜的，
// 与服务端实现对不上（那样头是 6 字节，且分片号用 2 字节没意义）。
// 这里以服务端源码为准：`rtcChunkHeader = 4`，注释写着
// 「2 字节帧序号 + 1 字节分片号 + 1 字节总片数」。
// 单分片帧也是同一条路径：total=1、index=0，不做「小帧免头」的特例。
// 单分片载荷上限 16KB（服务端 rtcChunkPayload），一帧 100KB 的 JPEG 大约 7 片。
//
// 2. **不配 STUN/TURN。** 服务端只跑 host candidate：局域网直连能通，
// 外网要靠 relay（docs/relay.md），打洞失败就回退 WS。配一个 STUN 只会让
// 外网场景多几秒的无效等待。
//
// 3. **非 trickle。** 客户端聚合完候选再发 offer（服务端也是聚合完才回 answer）。
// 浏览器会把候选随收集逐步写进 localDescription，所以「超时就先用当前 SDP」
// 是安全的：拿到的候选照样在 SDP 里。
//
// 硬约束：**任何一步失败都只回退到 WS 的 binary 帧，不弹错、不断流。**
// 服务端在 rtc-answer 之后仍会继续发 binary 帧（见 internal/api/remote_ws.go
// 的文件头注释），所以回退永远有画面。

/** 分片头长度（字节）。与后端 rtcChunkHeader 一致。 */
export const FRAME_CHUNK_HEADER = 4
/** 单分片载荷上限。与后端 rtcChunkPayload 一致，只用于解释日志。 */
export const FRAME_CHUNK_PAYLOAD = 16 << 10
/** 分片数上限。与后端 rtcMaxChunks 一致（头里 total 只有 1 字节）。 */
export const MAX_FRAME_CHUNKS = 255

/** 等 ICE 收集完成的上限。服务端是 5s，客户端更短：慢就先用当前 SDP。 */
export const GATHER_TIMEOUT_MS = 3000
/** 等 rtc-answer / rtc-failed 的上限。服务端处理一次 offer 最长 20s，
 *  但它的 ICE 收集本身只等 5s——所以这里也取 5s。
 *  卡在边界上（服务端恰好 5s 才回）会白丢一次协商，代价只是这次不用 RTC：
 *  回退是静默的，WS 帧一秒都没断。 */
export const ANSWER_TIMEOUT_MS = 5000
/** 等 frames 通道打开的上限。与后端 rtcOpenTimeout 同为 10s。 */
export const OPEN_TIMEOUT_MS = 10_000
/** 一帧没收完的等待上限。与后端 assembleTimeout 一致。 */
export const ASSEMBLE_TIMEOUT_MS = 2000

export type FrameChunk = {
  seq: number
  index: number
  total: number
  payload: Uint8Array
}

/**
 * 解析一条 frames 消息。合法返回分片，非法返回 null（调用方丢这一条即可）。
 *
 * 判定顺序按服务端 decodeChunkHeader：长度不足 4 或 total=0 / index>=total
 * 一律非法。额外要求载荷非空：4 字节空载荷拼出来的「帧」喂给解码器只会
 * 白报一次错。
 */
export function decodeFrameChunk(buf: ArrayBuffer | Uint8Array): FrameChunk | null {
  const u8 = buf instanceof Uint8Array ? buf : new Uint8Array(buf)
  if (u8.byteLength <= FRAME_CHUNK_HEADER) return null
  const seq = (u8[0] << 8) | u8[1]
  const index = u8[2]
  const total = u8[3]
  if (total === 0 || index >= total) return null
  return { seq, index, total, payload: u8.subarray(FRAME_CHUNK_HEADER) }
}

export type ChunkDrop = 'short' | 'timeout' | 'incomplete' | 'out-of-order'

/**
 * 帧重组器。逐条照搬服务端 frameAssembler 的判定顺序：
 * 先判半成品超时，再判「换帧了」，最后判乱序。
 *
 * 顺序不能换：上一帧缺片时，下一帧的第一片必然「序号不同」，
 * 先判序号会把「缺片」误报成「乱序」，日志就没法看了。
 *
 * 一条 DC 是可靠且有序的（service 端建的是 ordered=true），所以正常情况下
 * 不会乱序；乱序只会在「客户端切网/切后台」造成的半帧时出现。
 */
export class FrameAssembler {
  private seq = -1
  private total = 0
  private next = 0
  private parts: Uint8Array[] = []
  private started = 0

  /** 成功重组的帧数。 */
  frames = 0
  /** 被丢掉的半成品数。 */
  drops = 0
  /** 最近一次丢弃的原因，排障用。 */
  lastDrop: ChunkDrop | null = null

  constructor(private readonly timeoutMs: number = ASSEMBLE_TIMEOUT_MS) {}

  private reset() {
    this.seq = -1
    this.total = 0
    this.next = 0
    this.parts = []
    this.started = 0
  }

  /** 喂一条分片；返回完整帧，没有就返回 null。 */
  push(buf: ArrayBuffer | Uint8Array, now: number = Date.now()): Uint8Array | null {
    const chunk = decodeFrameChunk(buf)
    if (!chunk) {
      this.reset()
      this.drops++
      this.lastDrop = 'short'
      return null
    }

    // 半成品超时：先判这个，否则「上一帧缺片」会表现成「乱序」。
    if (this.total > 0 && this.started > 0 && now - this.started > this.timeoutMs) {
      this.reset()
      this.drops++
      this.lastDrop = 'timeout'
      return null
    }

    if (this.total === 0) {
      this.seq = chunk.seq
      this.total = chunk.total
      this.next = 0
      this.parts = []
      this.started = now
    } else if (chunk.seq !== this.seq) {
      this.reset()
      this.drops++
      this.lastDrop = 'incomplete'
      return null
    }

    if (chunk.index !== this.next) {
      this.reset()
      this.drops++
      this.lastDrop = 'out-of-order'
      return null
    }

    this.parts.push(chunk.payload)
    this.next++

    if (this.next === this.total) {
      const frame = concat(this.parts)
      this.reset()
      this.frames++
      return frame
    }
    return null
  }
}

function concat(parts: Uint8Array[]): Uint8Array {
  let len = 0
  for (const p of parts) len += p.byteLength
  const out = new Uint8Array(len)
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.byteLength
  }
  return out
}

// ── 协商 ────────────────────────────────────────────────────────

export type PeerCtor = new (config: RTCConfiguration) => RTCPeerConnection

export type WebRTCSignal = { type?: string; sdp?: string; reason?: string }

export type WebRTCEvent =
  /** frames 通道打开，可以用这条路出画/收输入了。 */
  | { kind: 'connect' }
  /** 这条路不通，回退 WS + JPEG。reason 只进日志/状态条 tooltip。 */
  | { kind: 'fallback'; reason: string }

export type StartWebRTCOptions = {
  /** 通过 WS 发信令。服务端只认 `{"type":"rtc-offer","sdp":..}`。 */
  sendSignal: (msg: { type: 'rtc-offer'; sdp: string }) => void
  /** 收到一帧完整 JPEG。 */
  onFrame: (jpeg: Uint8Array) => void
  onEvent: (ev: WebRTCEvent) => void
  /** 注入点：单测用假的 PeerConnection。 */
  Peer?: PeerCtor
  gatherTimeoutMs?: number
  answerTimeoutMs?: number
  openTimeoutMs?: number
}

export type WebRTCSession = {
  /** 把 WS 上的 rtc-answer / rtc-failed 转进来。 */
  handleSignal: (msg: WebRTCSignal) => void
  /** 输入走 input DC；返回 false 表示这条路还没通，调用方应回退到 WS。 */
  sendInput: (ev: unknown) => boolean
  close: () => void
  /** negotiating = 协商中（画面仍走 WS）；rtc = 已连通；ws = 已回退。 */
  mode: () => 'negotiating' | 'rtc' | 'ws'
}

/**
 * 起一次协商。函数立即返回句柄 —— 协商是异步的，调用方要在 WS 上把
 * rtc-answer / rtc-failed 转发给 handleSignal。
 *
 * 生命周期：一条 WS 对应一个 session。断线重连时**必须**重新建一个
 * （PeerConnection 不能复用：旧 PC 的 ICE 状态、DTLS 指纹、DC 全都作废）。
 */
export function startWebRTC(opts: StartWebRTCOptions): WebRTCSession {
  const gatherTimeoutMs = opts.gatherTimeoutMs ?? GATHER_TIMEOUT_MS
  const answerTimeoutMs = opts.answerTimeoutMs ?? ANSWER_TIMEOUT_MS
  const openTimeoutMs = opts.openTimeoutMs ?? OPEN_TIMEOUT_MS

  let mode: 'negotiating' | 'rtc' | 'ws' = 'negotiating'
  let closed = false
  let pc: RTCPeerConnection | null = null
  let input: RTCDataChannel | null = null
  let answerTimer: number | undefined
  let openTimer: number | undefined
  const assembler = new FrameAssembler()

  const clearTimers = () => {
    if (answerTimer !== undefined) clearTimeout(answerTimer)
    if (openTimer !== undefined) clearTimeout(openTimer)
    answerTimer = undefined
    openTimer = undefined
  }

  /** 回退：只发一次，之后不再改 mode。close() 之后静默。 */
  const fallback = (reason: string) => {
    if (mode === 'ws' || closed) return
    mode = 'ws'
    clearTimers()
    // 只关 PC，不关 WS —— WS 那条路是回退路径，必须留着。
    pc?.close()
    pc = null
    input = null
    opts.onEvent({ kind: 'fallback', reason })
  }

  const Peer = opts.Peer ?? (globalThis as { RTCPeerConnection?: PeerCtor }).RTCPeerConnection
  if (!Peer) {
    // 老浏览器 / 非安全上下文（http 页面里 RTCPeerConnection 会不可用）。
    fallback('浏览器不支持 WebRTC')
    return {
      handleSignal: () => {},
      sendInput: () => false,
      close: () => {},
      mode: () => 'ws',
    }
  }

  let conn: RTCPeerConnection
  try {
    // iceServers 故意留空：见文件头第 2 点。
    conn = new Peer({ iceServers: [] })
  } catch (e) {
    fallback(`创建 PeerConnection 失败：${errText(e)}`)
    return {
      handleSignal: () => {},
      sendInput: () => false,
      close: () => {},
      mode: () => 'ws',
    }
  }
  pc = conn

  conn.onconnectionstatechange = () => {
    const s = conn.connectionState
    if (s === 'failed' || s === 'closed') fallback(`连接状态 ${s}`)
  }

  // 画面通道由**服务端**创建（它要先于 answer 进 SDP），这边只能靠接。
  conn.ondatachannel = (ev: RTCDataChannelEvent) => {
    const dc = ev.channel
    if (dc.label !== 'frames') {
      // 不认识的通道直接关，别占着 SCTP 流（服务端也是这么处理 input 之外的同名逻辑）。
      try {
        dc.close()
      } catch {
        /* 已经关了 */
      }
      return
    }
    dc.binaryType = 'arraybuffer'
    dc.onmessage = (m: MessageEvent) => {
      const data = m.data
      if (typeof data === 'string') return
      if (!(data instanceof ArrayBuffer) && !(data instanceof Uint8Array)) return
      const frame = assembler.push(data)
      if (frame && frame.byteLength > 0) opts.onFrame(frame)
    }
    dc.onopen = () => {
      if (closed || mode === 'ws') return
      mode = 'rtc'
      clearTimers()
      opts.onEvent({ kind: 'connect' })
    }
    dc.onclose = () => {
      // 通道自己关了 = 这条路没了。回退（WS 还在出图）。
      if (mode === 'rtc') fallback('frames 通道关闭')
    }
  }

  const negotiate = async () => {
    // input 通道必须由客户端建：服务端只会创建 frames，并在 OnDataChannel
    // 里认领 label=input。建在 offer 之前，否则 offer 里没有 m=application，
    // 服务端会直接回 rtc-failed（见 webrtc.go 的 m=application 检查）。
    try {
      input = conn.createDataChannel('input', { ordered: true })
    } catch (e) {
      fallback(`创建 input 通道失败：${errText(e)}`)
      return
    }

    let offer: RTCSessionDescriptionInit
    try {
      offer = await conn.createOffer()
      await conn.setLocalDescription(offer)
    } catch (e) {
      fallback(`createOffer 失败：${errText(e)}`)
      return
    }
    if (closed) return

    const gathered = await waitIceGathering(conn, gatherTimeoutMs)
    if (closed) return

    // 非 trickle：候选在 localDescription 里。取不到就用手上的 offer。
    const sdp = conn.localDescription?.sdp || offer.sdp || ''
    if (!sdp) {
      fallback('本地 SDP 为空')
      return
    }
    if (gathered === 'timeout') {
      // 不是错误：局域网 host 候选通常几十毫秒就齐了，超时只是「不等了」。
      // 但完全没候选时打洞必失败，早点回退省得白等 10 秒。
      if (!/a=candidate:/.test(sdp)) {
        fallback(`${gatherTimeoutMs}ms 内没收集到 ICE 候选`)
        return
      }
    }

    opts.sendSignal({ type: 'rtc-offer', sdp })

    answerTimer = setTimeout(() => {
      fallback('等 rtc-answer 超时')
    }, answerTimeoutMs)
  }

  void negotiate()

  const armOpenTimer = () => {
    if (openTimer !== undefined) clearTimeout(openTimer)
    openTimer = setTimeout(() => {
      if (mode === 'negotiating') fallback('frames 通道一直没打开（ICE 未连通）')
    }, openTimeoutMs)
  }

  return {
    handleSignal(msg) {
      if (closed || mode === 'ws') return
      if (msg.type === 'rtc-failed') {
        fallback(msg.reason || '服务端拒绝 WebRTC')
        return
      }
      if (msg.type !== 'rtc-answer') return
      if (!msg.sdp || !pc) {
        fallback('rtc-answer 缺少 sdp')
        return
      }
      if (answerTimer !== undefined) {
        clearTimeout(answerTimer)
        answerTimer = undefined
      }
      pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp }).then(
        () => armOpenTimer(),
        (e) => fallback(`应用 answer 失败：${errText(e)}`),
      )
    },

    sendInput(ev) {
      if (mode !== 'rtc' || !input || input.readyState !== 'open') return false
      try {
        // 服务端把这条 DC 的消息直接 json.Unmarshal 成 InputEvent，
        // 所以这里**不能**带 `{"type":"input"}` 外壳（WS 才需要）。
        input.send(JSON.stringify(ev))
        return true
      } catch {
        return false
      }
    },

    close() {
      if (closed) return
      closed = true
      clearTimers()
      try {
        pc?.close()
      } catch {
        /* 已关 */
      }
      pc = null
      input = null
    },

    mode: () => mode,
  }
}

/**
 * 等 ICE 收集完成。返回 'complete' / 'timeout'（超时不是错误，见调用处）。
 *
 * 两条路都挂上：onicegatheringstatechange（真实浏览器）+ 100ms 轮询。
 * 轮询是为了「事件在挂监听之前就发生完了」这种竞态——它不会发生，但成本
 * 是一次计时器，而漏掉的后果是干等 3 秒。单测里的假 PC 也靠轮询驱动。
 */
export function waitIceGathering(
  pc: Pick<RTCPeerConnection, 'iceGatheringState'>,
  timeoutMs: number,
): Promise<'complete' | 'timeout'> {
  if (pc.iceGatheringState === 'complete') return Promise.resolve('complete')
  return new Promise((resolve) => {
    let done = false
    const sameOrigin = pc as unknown as {
      addEventListener?: (t: string, h: () => void) => void
      removeEventListener?: (t: string, h: () => void) => void
    }
    const finish = (how: 'complete' | 'timeout') => {
      if (done) return
      done = true
      clearInterval(timer)
      clearTimeout(deadline)
      sameOrigin.removeEventListener?.('icegatheringstatechange', check)
      resolve(how)
    }
    const check = () => {
      if (pc.iceGatheringState === 'complete') finish('complete')
    }
    const timer = setInterval(check, 100)
    const deadline = setTimeout(() => finish('timeout'), timeoutMs)
    sameOrigin.addEventListener?.('icegatheringstatechange', check)
  })
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
