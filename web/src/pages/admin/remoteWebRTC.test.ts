// WebRTC 链路的纯逻辑断言：分片重组 + 非 trickle 协商 + 回退判定。
//
// 为什么这些必须钉住：
// - 分片头是「4 字节：2 字节 seq + 1 字节 index + 1 字节 total」，写错一位
//   的表现是「偶尔花屏/整帧丢弃」，在真机上极难复现；
// - 协商超时如果不回退，用户看到的是「画面卡在最后一帧」而不是「降级成 WS」，
//   这两种状态的排障方向完全不同；
// - 回退必须是**静默**的：任何一条路径上抛错都会让远端会话看起来挂了。
//
// 用假的 RTCPeerConnection 驱动，不依赖浏览器。

import { describe, expect, it } from 'vitest'
import {
  FRAME_CHUNK_HEADER,
  FrameAssembler,
  decodeFrameChunk,
  startWebRTC,
  waitIceGathering,
} from './remoteWebRTC'
import type { PeerCtor, WebRTCEvent } from './remoteWebRTC'

// ── 分片头的构造（与 internal/remotedesk/webrtc.go 的 encodeChunkHeader 同构）──

function chunk(seq: number, index: number, total: number, payload: number[]): Uint8Array {
  const out = new Uint8Array(FRAME_CHUNK_HEADER + payload.length)
  out[0] = (seq >> 8) & 0xff
  out[1] = seq & 0xff
  out[2] = index
  out[3] = total
  out.set(payload, FRAME_CHUNK_HEADER)
  return out
}

describe('decodeFrameChunk：4 字节头', () => {
  it('单分片：seq 大端、index 0、total 1、载荷从第 4 字节开始', () => {
    const c = decodeFrameChunk(chunk(0x1234, 0, 1, [0xff, 0xd8, 0xff, 0xd9]))
    expect(c).not.toBeNull()
    expect(c!.seq).toBe(0x1234) // 大端：0x12 是高字节
    expect(c!.index).toBe(0)
    expect(c!.total).toBe(1)
    expect(Array.from(c!.payload)).toEqual([0xff, 0xd8, 0xff, 0xd9])
  })

  it('多分片：最大分片号 254（total=255）仍然合法', () => {
    const c = decodeFrameChunk(chunk(7, 254, 255, [1, 2]))
    expect(c!.index).toBe(254)
    expect(c!.total).toBe(255)
  })

  it('长度不足 4、或者只有头没有载荷 → null（不猜）', () => {
    expect(decodeFrameChunk(new Uint8Array([0, 1, 0]))).toBeNull()
    expect(decodeFrameChunk(chunk(1, 0, 1, []))).toBeNull()
  })

  it('total=0 与 index>=total 都是非法头（后端同样拒绝）', () => {
    expect(decodeFrameChunk(chunk(1, 0, 0, [9]))).toBeNull()
    expect(decodeFrameChunk(chunk(1, 2, 2, [9]))).toBeNull()
  })

  it('接受 ArrayBuffer 也接受 Uint8Array（WS/DC 两种入口）', () => {
    const u8 = chunk(3, 0, 1, [42])
    const buf = u8.buffer.slice(u8.byteOffset, u8.byteOffset + u8.byteLength) as ArrayBuffer
    expect(decodeFrameChunk(buf)!.payload[0]).toBe(42)
    expect(decodeFrameChunk(u8)!.payload[0]).toBe(42)
  })
})

describe('FrameAssembler：单帧内多分片', () => {
  it('1 片就能出帧', () => {
    const a = new FrameAssembler()
    const frame = a.push(chunk(0, 0, 1, [1, 2, 3]), 1000)
    expect(Array.from(frame!)).toEqual([1, 2, 3])
    expect(a.frames).toBe(1)
    expect(a.drops).toBe(0)
  })

  it('3 片按序拼回原字节，中途不提前出帧', () => {
    const a = new FrameAssembler()
    expect(a.push(chunk(5, 0, 3, [1, 2]), 1000)).toBeNull()
    expect(a.push(chunk(5, 1, 3, [3]), 1010)).toBeNull()
    const frame = a.push(chunk(5, 2, 3, [4, 5]), 1020)
    expect(Array.from(frame!)).toEqual([1, 2, 3, 4, 5])
    expect(a.frames).toBe(1)
  })

  it('连续两帧互不污染（seq 递增，半成品已清空）', () => {
    const a = new FrameAssembler()
    a.push(chunk(1, 0, 2, [0xa]), 1000)
    a.push(chunk(1, 1, 2, [0xb]), 1000)
    const second = a.push(chunk(2, 0, 1, [0xc]), 1000)
    expect(Array.from(second!)).toEqual([0xc])
    expect(a.frames).toBe(2)
  })
})

describe('FrameAssembler：乱序与缺片', () => {
  it('乱序（先到 index 1）→ 整帧作废，不拼错位数据', () => {
    const a = new FrameAssembler()
    a.push(chunk(9, 0, 3, [1]), 1000)
    expect(a.push(chunk(9, 2, 3, [3]), 1000)).toBeNull()
    expect(a.lastDrop).toBe('out-of-order')
    expect(a.drops).toBe(1)
    expect(a.frames).toBe(0)
  })

  it('缺片：上一帧没收完就来了下一帧 → 丢上一帧，从新帧重新开始', () => {
    const a = new FrameAssembler()
    a.push(chunk(10, 0, 2, [1]), 1000) // 缺 index 1
    // 下一帧的第一片：seq 变了，且半成品还没超时
    expect(a.push(chunk(11, 0, 2, [7]), 1100)).toBeNull()
    expect(a.lastDrop).toBe('incomplete')
    expect(a.drops).toBe(1)
    // 被拒的那一片把重组器清空了，所以 seq=11 这一帧也废了（与后端一致：
    // 宁可丢一帧，也不要把错位的数据拼给解码器）。下一帧从头开始就正常。
    expect(a.push(chunk(11, 1, 2, [8]), 1110)).toBeNull()
    const frame = a.push(chunk(12, 0, 2, [7]), 1120)
    expect(frame).toBeNull()
    expect(Array.from(a.push(chunk(12, 1, 2, [8]), 1130)!)).toEqual([7, 8])
    expect(a.frames).toBe(1)
  })

  it('半成品超时（>2s）→ 丢弃并计 drops，之后仍能收新帧', () => {
    const a = new FrameAssembler(2000)
    a.push(chunk(20, 0, 2, [1]), 1000)
    expect(a.push(chunk(20, 1, 2, [2]), 3101)).toBeNull() // 超时优先于「序号一致」
    expect(a.lastDrop).toBe('timeout')
    const frame = a.push(chunk(21, 0, 1, [5]), 3200)
    expect(Array.from(frame!)).toEqual([5])
  })

  it('重复分片（index 回退）按乱序处理，不把同一片拼两次', () => {
    const a = new FrameAssembler()
    a.push(chunk(30, 0, 2, [1]), 1000)
    expect(a.push(chunk(30, 0, 2, [1]), 1000)).toBeNull()
    expect(a.lastDrop).toBe('out-of-order')
    expect(a.frames).toBe(0)
  })

  it('非法头（长度不足/载荷为空）不会污染后续重组', () => {
    const a = new FrameAssembler()
    a.push(chunk(40, 0, 2, [1]), 1000)
    expect(a.push(new Uint8Array([0, 1]), 1000)).toBeNull()
    expect(a.lastDrop).toBe('short')
    // 半成品被清掉 → 这一帧也废了，下一帧从零开始
    expect(a.push(chunk(40, 1, 2, [2]), 1000)).toBeNull()
    expect(a.push(chunk(41, 0, 1, [3]), 1000)).not.toBeNull()
  })
})

// ── 假的 RTCPeerConnection ─────────────────────────────────────
//
// 假的只实现被测代码真正碰到的成员：iceGatheringState / localDescription /
// ondatachannel / createOffer / setLocalDescription / setRemoteDescription / close。
// 「候选逐步写进 localDescription」这一条要模拟准：非 trickle 的成败全靠它。

type FakeOpts = {
  /** 'never' = ICE 永远不完成（用来测超时）。 */
  gather?: 'fast' | 'never'
  /** 打到 complete 之前先塞进 localDescription 的候选行。 */
  candidates?: string[]
  /** createOffer 抛错（老浏览器/权限问题）。 */
  failOffer?: boolean
}

let fakeOpts: FakeOpts = {}

class FakePC {
  static last: FakePC | null = null

  iceGatheringState = 'new'
  connectionState = 'new'
  localDescription: { type: string; sdp: string } | null = null
  ondatachannel: ((ev: { channel: FakeDC }) => void) | null = null
  onconnectionstatechange: (() => void) | null = null
  closed = false
  readonly config: RTCConfiguration | undefined
  channels: FakeDC[] = []
  private listeners = new Set<() => void>()

  constructor(config?: RTCConfiguration) {
    this.config = config
    FakePC.last = this
  }

  addEventListener(t: string, h: () => void) {
    if (t === 'icegatheringstatechange') this.listeners.add(h)
  }

  removeEventListener(t: string, h: () => void) {
    if (t === 'icegatheringstatechange') this.listeners.delete(h)
  }

  createDataChannel(label: string) {
    const dc = new FakeDC(label)
    this.channels.push(dc)
    return dc
  }

  async createOffer() {
    if (fakeOpts.failOffer) throw new Error('createOffer 崩了')
    // 有 m=application 才像真的 offer（服务端会检查它）
    return { type: 'offer', sdp: 'v=0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n' }
  }

  async setLocalDescription(d: { type: string; sdp: string }) {
    // 真实浏览器把候选逐步写进 localDescription —— 即使「没等到 complete」
    // 也已经有一部分，这正是「超时就先用当前 SDP」能成立的前提。
    this.localDescription = d
    for (const c of fakeOpts.candidates ?? []) {
      this.localDescription = { ...this.localDescription, sdp: `${this.localDescription.sdp}a=${c}\r\n` }
    }
    if (fakeOpts.gather === 'never') return
    this.iceGatheringState = 'complete'
    for (const l of this.listeners) l()
  }

  async setRemoteDescription(_d: { type: string; sdp: string }) {
    /* 只记不校验 */
  }

  /** 测试用：模拟服务端建好 frames 通道。 */
  deliverFramesChannel() {
    const dc = new FakeDC('frames')
    this.channels.push(dc)
    this.ondatachannel?.({ channel: dc })
    return dc
  }

  close() {
    this.closed = true
  }
}

class FakeDC {
  binaryType = 'blob'
  readyState = 'connecting'
  onmessage: ((m: { data: unknown }) => void) | null = null
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  sent: string[] = []
  constructor(readonly label: string) {}

  send(data: string) {
    this.sent.push(data)
  }
  close() {
    this.readyState = 'closed'
    this.onclose?.()
  }
  /** 测试用：打开通道并触发回调。 */
  open() {
    this.readyState = 'open'
    this.onopen?.()
  }
}

function peerOf(opts: FakeOpts = {}): PeerCtor {
  fakeOpts = opts
  return FakePC as unknown as PeerCtor
}

const flush = () => new Promise((r) => setTimeout(r, 0))

describe('waitIceGathering', () => {
  it('已经是 complete 立刻返回', async () => {
    await expect(waitIceGathering({ iceGatheringState: 'complete' } as never, 50)).resolves.toBe(
      'complete',
    )
  })

  it('一直不完成 → 超时返回 timeout（不是 rejects）', async () => {
    await expect(waitIceGathering({ iceGatheringState: 'gathering' } as never, 40)).resolves.toBe(
      'timeout',
    )
  })
})

describe('startWebRTC：非 trickle 协商', () => {
  it('offer 里带上已收集的候选，并且只发一条 rtc-offer', async () => {
    const signals: Array<{ type: string; sdp: string }> = []
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf({ candidates: ['candidate:1 1 udp 2130706431 192.168.1.9 51234 typ host'] }),
      sendSignal: (m) => signals.push(m),
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 200,
      answerTimeoutMs: 200,
    })
    await flush()

    expect(signals).toHaveLength(1)
    expect(signals[0].type).toBe('rtc-offer')
    expect(signals[0].sdp).toContain('a=candidate:')
    // 不配 STUN/TURN：外网靠 relay，打洞靠 host candidate
    expect(FakePC.last!.config).toEqual({ iceServers: [] })
    // 还没收到 answer，不算失败
    expect(events).toHaveLength(0)
    expect(session.mode()).toBe('negotiating')
    session.close()
  })

  it('ICE 收集超时但已有候选 → 照样发 offer（不等满）', async () => {
    const signals: Array<{ sdp: string }> = []
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      // gather=never 但候选已经在 SDP 里（模拟「收集慢但仍可用」）
      Peer: peerOf({ gather: 'never', candidates: ['candidate:2 1 udp 1 10.0.0.2 6000 typ host'] }),
      sendSignal: (m) => signals.push(m),
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 40,
    })
    await new Promise((r) => setTimeout(r, 80))

    expect(signals).toHaveLength(1)
    expect(signals[0].sdp).toContain('candidate:2')
    expect(events).toHaveLength(0) // 超时本身不算失败
    session.close()
  })

  it('ICE 收集超时且一个候选都没有 → 直接回退，不再等 answer', async () => {
    const signals: Array<unknown> = []
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf({ gather: 'never' }),
      sendSignal: (m) => signals.push(m),
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 40,
    })
    await new Promise((r) => setTimeout(r, 80))

    expect(signals).toHaveLength(0)
    expect(events).toEqual([{ kind: 'fallback', reason: expect.stringContaining('ICE') }])
    expect(session.mode()).toBe('ws')
  })

  it('createOffer 抛错 → 回退，并且不抛给调用方', async () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf({ failOffer: true }),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
    })
    await flush()
    expect(events[0].kind).toBe('fallback')
    expect(session.mode()).toBe('ws')
  })

  it('没有 RTCPeerConnection 的环境（http/老浏览器）→ 立刻回退', () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      // 显式传 undefined：模拟 globalThis 上没有这个构造器
      Peer: undefined as unknown as PeerCtor,
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
    })
    // 真 Node 环境本来就没有 RTCPeerConnection，所以这里拿到的就是回退
    expect(events).toHaveLength(1)
    expect(events[0].kind).toBe('fallback')
    expect(session.sendInput({ op: 'click' })).toBe(false)
    expect(session.mode()).toBe('ws')
  })
})

describe('startWebRTC：answer → frames 通道 → 出帧', () => {
  it('收 answer → frames 打开 → connect；分片能拼回整帧', async () => {
    const events: WebRTCEvent[] = []
    const frames: Uint8Array[] = []
    const session = startWebRTC({
      Peer: peerOf({ candidates: ['candidate:3 1 udp 1 192.168.0.5 7000 typ host'] }),
      sendSignal: () => {},
      onFrame: (f) => frames.push(f),
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 100,
      answerTimeoutMs: 200,
    })
    await flush()
    const pc = FakePC.last!

    session.handleSignal({ type: 'rtc-answer', sdp: 'v=0\r\n' })
    await flush()

    const dc = pc.deliverFramesChannel()
    dc.open()
    expect(events).toEqual([{ kind: 'connect' }])
    expect(session.mode()).toBe('rtc')

    // 两片一条帧
    dc.onmessage!({ data: chunk(1, 0, 2, [0xff, 0xd8]).buffer })
    dc.onmessage!({ data: chunk(1, 1, 2, [0xff, 0xd9]).buffer })
    expect(frames).toHaveLength(1)
    expect(Array.from(frames[0])).toEqual([0xff, 0xd8, 0xff, 0xd9])

    // 半帧不会吐给解码器
    dc.onmessage!({ data: chunk(2, 0, 2, [1]).buffer })
    expect(frames).toHaveLength(1)

    session.close()
    expect(pc.closed).toBe(true)
  })

  it('rtc-failed → 静默回退，mode 变 ws，不再改回来', async () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
    })
    await flush()
    session.handleSignal({ type: 'rtc-failed', reason: '本机不支持 WebRTC' })
    expect(events).toEqual([{ kind: 'fallback', reason: '本机不支持 WebRTC' }])
    expect(session.mode()).toBe('ws')

    // 之后的 answer 不该把它救回来（PC 已经关了）
    session.handleSignal({ type: 'rtc-answer', sdp: 'v=0\r\n' })
    expect(session.mode()).toBe('ws')
    expect(events).toHaveLength(1)
  })

  it('answer 超时 → 回退', async () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 100,
      answerTimeoutMs: 40,
    })
    await new Promise((r) => setTimeout(r, 220))
    expect(events).toEqual([{ kind: 'fallback', reason: expect.stringContaining('answer') }])
    expect(session.mode()).toBe('ws')
  })

  it('answer 到了但 frames 通道 10 秒没打开 → 回退（openTimeout 可注入）', async () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 100,
      answerTimeoutMs: 200,
      openTimeoutMs: 40,
    })
    await flush()
    session.handleSignal({ type: 'rtc-answer', sdp: 'v=0\r\n' })
    await new Promise((r) => setTimeout(r, 90))
    expect(events).toEqual([{ kind: 'fallback', reason: expect.stringContaining('frames') }])
    expect(session.mode()).toBe('ws')
  })

  it('connect 之后通道关闭 → 也回退（WS 还在出图）', async () => {
    const events: WebRTCEvent[] = []
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: (e) => events.push(e),
      gatherTimeoutMs: 100,
      answerTimeoutMs: 200,
    })
    await flush()
    session.handleSignal({ type: 'rtc-answer', sdp: 'v=0\r\n' })
    await flush()
    const dc = FakePC.last!.deliverFramesChannel()
    dc.open()
    expect(session.mode()).toBe('rtc')

    dc.close()
    expect(session.mode()).toBe('ws')
    expect(events.map((e) => e.kind)).toEqual(['connect', 'fallback'])
    session.close()
  })

  it('不再需要的通道（label 不是 frames）被关掉，不占 SCTP 流', async () => {
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: () => {},
      gatherTimeoutMs: 100,
    })
    await flush()
    const pc = FakePC.last!
    const stray = new FakeDC('chat')
    pc.ondatachannel?.({ channel: stray })
    expect(stray.readyState).toBe('closed')
    session.close()
  })
})

describe('startWebRTC：输入通道', () => {
  it('连上之前 sendInput 返回 false（调用方回退 WS）', async () => {
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: () => {},
      gatherTimeoutMs: 100,
    })
    await flush()
    expect(session.sendInput({ op: 'click', x: 0.5, y: 0.5 })).toBe(false)
    session.close()
  })

  it('连上之后发的是扁平 InputEvent（没有 type 外壳），服务端才认得', async () => {
    const session = startWebRTC({
      Peer: peerOf(),
      sendSignal: () => {},
      onFrame: () => {},
      onEvent: () => {},
      gatherTimeoutMs: 100,
    })
    await flush()
    session.handleSignal({ type: 'rtc-answer', sdp: 'v=0\r\n' })
    await flush()
    const framesDC = FakePC.last!.deliverFramesChannel()
    framesDC.open()

    const inputDC = FakePC.last!.channels.find((c) => c.label === 'input')!
    inputDC.readyState = 'open'

    expect(session.sendInput({ op: 'move', x: 0.25, y: 0.75 })).toBe(true)
    expect(JSON.parse(inputDC.sent[0])).toEqual({ op: 'move', x: 0.25, y: 0.75 })
    expect(inputDC.sent[0]).not.toContain('"type"')
    session.close()
  })
})
