// 远程画面查看器：一条 WebSocket 单帧 JPEG 流 + canvas 渲染 + 手势注入。
//
// 三件容易做错的事，先写在这里：
//
// 1. **每帧必须 bitmap.close()。** ImageBitmap 不归 GC 管（解码后的像素在
//    浏览器进程里），手机上 8fps × 5 分钟就是几百张 1600×900 的位图，
//    不 close 会直接被系统杀掉标签页。
// 2. **丢旧帧，不排队。** 解码比收帧慢时，排队只会让延迟越积越大。
//    这里只保留「最新一帧」，正在解码时来的帧直接覆盖上一张待解码的。
// 3. **票据是一次性的。** WS 不能带 header，所以每次（重）连都要重新
//    POST /v1/auth/sse-ticket 换票；复用旧票会被服务端 401 掉，
//    表现是「断线后再也连不上」。
//
// 另外：离开页面**不**主动结束会话（见 endNow 的注释），只关 WS。

import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, remoteApi, remoteStreamUrl, sseTicket } from '../../api'
import type { RemoteCapabilities, RemoteSession } from '../../api'
import { copyText } from '../../components/ui'
import {
  DEFAULT_TIER,
  QUALITY_PRESETS,
  actionsBlockedReason,
  dropRate,
  formatDuration,
  formatFps,
  isTerminalStatus,
  modeLabel,
  qualityMessage,
  readOnlyReason,
  reconnectDelay,
  resolutionText,
  sessionStatusLabel,
  sessionStatusTone,
} from './remoteModel'
import type { QualityTier } from './remoteModel'
import {
  DRAG_SLOP,
  PINCH_RATIO,
  detectGesture,
  wheelNotches,
} from './remoteGestures'
import type { GestureTrack, Point, RemoteInputEvent } from './remoteGestures'
import { normalizePoint } from './remoteModel'

/** 等桌面端批准的轮询：2s 一次，最多 60s。 */
const APPROVAL_POLL_MS = 2000
const APPROVAL_TIMEOUT_MS = 60_000
/** 触摸后多久内忽略合成鼠标事件（移动浏览器会在 touchend 后补发一套）。 */
const TOUCH_MOUSE_GUARD_MS = 800

type Conn = 'connecting' | 'live' | 'retry' | 'closed'

type Props = {
  initial: RemoteSession
  capabilities: RemoteCapabilities | null
  /** 状态变化回传，页面用它刷新会话区。 */
  onSession: (s: RemoteSession) => void
  /** 会话结束（用户点断开 / 服务端终结 / 等待超时）。 */
  onExit: (reason: string) => void
}

function isUnavailable(e: unknown): boolean {
  return e instanceof ApiError && e.status === 503
}

export function RemoteViewer({ initial, capabilities, onSession, onExit }: Props) {
  const [cur, setCur] = useState<RemoteSession>(initial)
  const [conn, setConn] = useState<Conn>('connecting')
  const [err, setErr] = useState('')
  const [notice, setNotice] = useState('')
  const [tier, setTier] = useState<QualityTier>(DEFAULT_TIER)
  const [zoom, setZoom] = useState(1)
  const [frame, setFrame] = useState({ w: 0, h: 0 })
  const [serverStats, setServerStats] = useState<RemoteSession['stats'] | null>(null)
  const [localFps, setLocalFps] = useState(0)
  // 会话时长的刷新钟：只为了每秒重渲染一次，别在渲染里直接 Date.now()
  // （那样只有别的状态变了才会跳秒）。
  const [clock, setClock] = useState(() => Date.now())
  const [waitLeft, setWaitLeft] = useState(APPROVAL_TIMEOUT_MS)
  const [actionMsg, setActionMsg] = useState('')
  const [actionPath, setActionPath] = useState('')
  const [keyText, setKeyText] = useState('')
  const [cmdMod, setCmdMod] = useState(false)
  const [appName, setAppName] = useState('')
  const [url, setUrl] = useState('')

  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const pendingRef = useRef<ArrayBuffer | null>(null)
  const drawingRef = useRef(false)
  const framesRef = useRef(0)
  const droppedRef = useRef(0)
  /** 双击记忆：上一次「轻点」的时刻与位置（client 坐标）。 */
  const tapRef = useRef<{ at: number; point: Point } | null>(null)
  const touchedAtRef = useRef(0)
  const tierRef = useRef<QualityTier>(DEFAULT_TIER)
  const zoomRef = useRef(1)

  // 会话对象换了（用户在会话区点了另一台设备）→ 整体重置。
  useEffect(() => {
    setCur(initial)
    setErr('')
    setNotice('')
    setServerStats(null)
    setFrame({ w: 0, h: 0 })
    framesRef.current = 0
    droppedRef.current = 0
  }, [initial])

  useEffect(() => {
    tierRef.current = tier
  }, [tier])
  useEffect(() => {
    zoomRef.current = zoom
  }, [zoom])

  const mode = cur.mode
  // 只读判定：只看模式，或服务端明确说本机不能注入键鼠。
  const readOnlyMsg = readOnlyReason(capabilities?.input ?? null, mode)
  const readOnly = readOnlyMsg !== ''
  const actionsWhy = actionsBlockedReason(capabilities?.actions ?? null)

  const send = useCallback((ev: RemoteInputEvent) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify({ type: 'input', ...ev }))
  }, [])

  const sendRaw = useCallback((msg: Record<string, unknown>) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify(msg))
  }, [])

  const sendQuality = useCallback(() => {
    sendRaw(qualityMessage(tierRef.current))
  }, [sendRaw])

  // ── 批准流程：control 会话默认要电脑点头 ────────────────────
  //
  // 2s 轮询而不是长连：这一段的时长以「几十秒」计，轮询足够，
  // 且不需要为它维持第二个实时通道。
  useEffect(() => {
    if (cur.status !== 'pending_approval') return
    let stopped = false
    setWaitLeft(APPROVAL_TIMEOUT_MS)
    const startedAt = Date.now()
    let ticks = 0
    const timer = window.setInterval(() => {
      ticks++
      setWaitLeft(Math.max(0, APPROVAL_TIMEOUT_MS - (Date.now() - startedAt)))
      if (ticks % Math.round(APPROVAL_POLL_MS / 1000) !== 0) return
      void remoteApi
        .getSession(cur.id)
        .then((s) => {
          if (stopped) return
          setCur(s)
          onSession(s)
          if (isTerminalStatus(s.status)) {
            window.clearInterval(timer)
            setNotice(
              s.status === 'denied'
                ? '电脑上拒绝了这次接管。'
                : `会话已结束${s.endReason ? `（${s.endReason}）` : ''}。`,
            )
          }
        })
        .catch((e) => {
          if (stopped) return
          if (isUnavailable(e)) setErr('远程桌面未启用（接口 503）')
        })
      if (Date.now() - startedAt > APPROVAL_TIMEOUT_MS) {
        window.clearInterval(timer)
        setNotice('等电脑确认超时（60 秒）。请在电脑上点「允许」，或改用「只看」。')
        void remoteApi.endSession(cur.id).catch(() => {})
      }
    }, 1000)
    return () => {
      stopped = true
      window.clearInterval(timer)
    }
  }, [cur.status, cur.id, onSession])

  // ── 帧解码：单通道，永远只处理最新一帧 ─────────────────────
  //
  // 循环里再取一次 pendingRef：解码一帧的时间足够再收一帧，
  // 那期间只保留最新的一张，旧的直接丢掉（丢帧计数器在 onmessage 里加）。
  const pump = useCallback(async () => {
    if (drawingRef.current) return
    drawingRef.current = true
    const cv = canvasRef.current
    const ctx = cv?.getContext('2d')
    try {
      while (pendingRef.current) {
        const buf = pendingRef.current
        pendingRef.current = null
        if (!cv || !ctx) break
        let bmp: ImageBitmap | null = null
        try {
          bmp = await createImageBitmap(new Blob([buf], { type: 'image/jpeg' }))
          if (cv.width !== bmp.width || cv.height !== bmp.height) {
            // canvas 内部尺寸跟帧走，CSS 尺寸由样式表控制 → 缩放交给浏览器。
            cv.width = bmp.width
            cv.height = bmp.height
            setFrame({ w: bmp.width, h: bmp.height })
          }
          ctx.drawImage(bmp, 0, 0)
          framesRef.current++
        } catch {
          /* 半帧、非 JPEG：丢掉，下一帧会补上 */
        } finally {
          // 关键：不 close 的话手机上每帧一张位图，几百帧就 OOM。
          bmp?.close()
        }
      }
    } finally {
      drawingRef.current = false
    }
  }, [])

  // ── WebSocket：连上 → 收帧 → 断线退避重连 ──────────────────
  useEffect(() => {
    if (cur.status !== 'active') return
    let closed = false
    let attempt = 0
    let timer: number | undefined
    let socket: WebSocket | null = null

    const schedule = () => {
      if (closed) return
      const delay = reconnectDelay(attempt)
      attempt++
      if (delay === null) {
        setConn('closed')
        setErr('连接中断，已重试 5 次仍失败。可以断开后重新连接。')
        return
      }
      setConn('retry')
      timer = window.setTimeout(() => void open(), delay)
    }

    const open = async () => {
      if (closed) return
      setConn(attempt === 0 ? 'connecting' : 'retry')
      let ticket = ''
      try {
        // 票只能用一次：每次重连都必须重新换。
        ticket = (await sseTicket()).ticket
      } catch (e) {
        if (isUnavailable(e)) {
          setErr('远程桌面未启用（接口 503）')
          return
        }
        schedule()
        return
      }
      if (closed) return
      socket = new WebSocket(remoteStreamUrl(cur.id, ticket))
      wsRef.current = socket
      socket.binaryType = 'arraybuffer'
      socket.onopen = () => {
        attempt = 0
        setConn('live')
        setErr('')
        sendQuality()
      }
      socket.onmessage = (ev: MessageEvent) => {
        if (typeof ev.data === 'string') {
          handleText(ev.data)
          return
        }
        if (!(ev.data instanceof ArrayBuffer)) return
        // 上一帧还在解码 → 覆盖它的前一张（丢旧帧）。
        if (pendingRef.current) droppedRef.current++
        pendingRef.current = ev.data
        void pump()
      }
      socket.onerror = () => {
        // onerror 之后一定会有 onclose，退避放在 close 里，避免 double-schedule。
      }
      socket.onclose = () => {
        wsRef.current = null
        if (closed) return
        // 断线有两种：网络抖一下，或者会话已经被结束（急停 / 设备被吊销）。
        // 先问一次状态：对着一个已经死掉的会话重试 5 次，用户只会看到
        // 「重连中…」转一分钟，其实是白等。
        void remoteApi
          .getSession(cur.id)
          .then((s) => {
            if (closed) return
            if (isTerminalStatus(s.status)) {
              setCur(s)
              onSession(s)
            } else {
              schedule()
            }
          })
          .catch(() => {
            if (!closed) schedule()
          })
      }
    }

    const handleText = (raw: string) => {
      let msg: any
      try {
        msg = JSON.parse(raw)
      } catch {
        return
      }
      switch (msg?.type) {
        case 'hello':
          if (msg.stats) setServerStats(normalizeStats(msg.stats))
          break
        case 'stats':
          // stats 可能直接平铺，也可能包在 stats 里；两种都认。
          setServerStats(normalizeStats(msg.stats ?? msg))
          break
        case 'error':
          setErr(String(msg.error || msg.detail || '画面流出错'))
          break
        case 'pong':
          break
        default:
          break
      }
    }

    void open()
    return () => {
      closed = true
      if (timer) window.clearTimeout(timer)
      // 先摘掉回调再关，避免 close 触发重连。
      if (socket) {
        socket.onclose = null
        socket.onopen = null
        socket.onmessage = null
        socket.onerror = null
        socket.close()
      }
      wsRef.current = null
      setConn('closed')
    }
    // pump 是 useCallback 的稳定引用；依赖只放会话与状态，
    // 避免每次 setState 都重连。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cur.status, cur.id, sendQuality, onSession])

  // 本地帧率 + 会话时长：1 秒刷一次，不每帧 setState。
  useEffect(() => {
    if (cur.status !== 'active') return
    let last = framesRef.current
    const timer = window.setInterval(() => {
      const now = framesRef.current
      setLocalFps(now - last)
      last = now
      setClock(Date.now())
    }, 1000)
    return () => window.clearInterval(timer)
  }, [cur.status])

  // ── 手势 ────────────────────────────────────────────────────
  //
  // 采集放在 ref 里（touchmove 每帧都跑，不能碰 state）。
  // 判定交给 remoteGestures.detectGesture（纯函数、有单测）；
  // 长按拖动必须**实时**发 down/move/up，那一小段在这里做。
  const gestureRef = useRef({
    startAt: 0,
    tracks: [] as GestureTrack[],
    active: new Map<number, GestureTrack>(),
    dragging: false,
    pinch: false,
    /** 已经在 move 里实时发过 wheel（手势结束时不再补发，否则会滚两次）。 */
    scrolled: false,
    pinchStart: 0,
    zoomStart: 1,
    scrollAccum: 0,
    lastScrollY: 0,
    holdTimer: 0,
    last: null as Point | null,
  })

  const resetGesture = () => {
    const g = gestureRef.current
    if (g.holdTimer) window.clearTimeout(g.holdTimer)
    g.holdTimer = 0
    g.startAt = 0
    g.tracks = []
    g.active.clear()
    g.dragging = false
    g.pinch = false
    g.scrolled = false
    g.pinchStart = 0
    g.scrollAccum = 0
    g.lastScrollY = 0
    g.last = null
  }

  useEffect(() => resetGesture, [])

  const rectOf = () => {
    const cv = canvasRef.current
    return cv ? cv.getBoundingClientRect() : { left: 0, top: 0, width: 0, height: 0 }
  }

  const applyGesture = (now: number) => {
    const g = gestureRef.current
    if (g.tracks.length === 0) return
    // 实时路径已经发过的部分不能再来一遍：捏合已经缩放过、滚轮已经逐档发过。
    const livePinch = g.pinch
    const liveScroll = g.scrolled
    const res = detectGesture({
      tracks: g.tracks,
      durationMs: Math.max(0, now - g.startAt),
      endAt: now,
      rect: rectOf(),
      lastTap: tapRef.current,
    })
    for (const ev of res.events) {
      if (liveScroll && ev.op === 'wheel') continue
      send(ev)
    }
    if (res.kind === 'tap') {
      const t = g.tracks[0]
      tapRef.current = { at: now, point: t.start }
    } else if (res.kind === 'double-tap') {
      // 用掉记忆，避免第三次轻点又变成双击。
      tapRef.current = null
    }
    if (res.kind === 'pinch' && !livePinch) {
      setZoom(clampZoom(zoomRef.current * res.scale))
    }
    resetGesture()
  }

  const startHold = () => {
    const g = gestureRef.current
    if (g.holdTimer) window.clearTimeout(g.holdTimer)
    g.holdTimer = window.setTimeout(() => {
      if (g.active.size !== 1) return
      const track = [...g.active.values()][0]
      g.dragging = true
      const p = normalizePoint(track.end.x, track.end.y, rectOf())
      send({ op: 'down', x: p.x, y: p.y })
    }, 600)
  }

  const onTouchStart = (e: React.TouchEvent) => {
    touchedAtRef.current = Date.now()
    if (readOnly) return
    e.preventDefault()
    const g = gestureRef.current
    if (g.active.size === 0) {
      // 新手势：清掉上一轮的轨迹（touchend 里已经 reset，这里兜底系统取消的情况）。
      g.tracks = []
      g.startAt = Date.now()
      g.pinch = false
      g.scrolled = false
      g.dragging = false
    }
    for (const t of Array.from(e.changedTouches)) {
      const point = { x: t.clientX, y: t.clientY }
      const track: GestureTrack = { start: point, end: point, samples: [point] }
      g.active.set(t.identifier, track)
      g.tracks.push(track)
    }
    if (g.active.size === 1) {
      startHold()
    } else if (g.active.size >= 2) {
      if (g.holdTimer) window.clearTimeout(g.holdTimer)
      g.holdTimer = 0
      const [a, b] = [...g.active.values()]
      g.pinchStart = Math.hypot(a.start.x - b.start.x, a.start.y - b.start.y)
      g.zoomStart = zoomRef.current
      g.lastScrollY = (a.start.y + b.start.y) / 2
    }
  }

  const onTouchMove = (e: React.TouchEvent) => {
    if (readOnly) return
    e.preventDefault()
    const g = gestureRef.current
    const changed = Array.from(e.changedTouches)
    for (const t of changed) {
      const track = g.active.get(t.identifier)
      if (!track) continue
      const point = { x: t.clientX, y: t.clientY }
      track.end = point
      // 采样点封顶：一次长时间拖动不该无限长（坐标换算用不到那么细）。
      if (!track.samples) track.samples = [track.start]
      if (track.samples.length < 120) track.samples.push(point)
      g.last = point
    }

    if (g.dragging) {
      if (g.last) {
        const p = normalizePoint(g.last.x, g.last.y, rectOf())
        send({ op: 'move', x: p.x, y: p.y })
      }
      return
    }

    if (g.active.size >= 2 && !g.pinch) {
      const [a, b] = [...g.active.values()]
      const d = Math.hypot(a.end.x - b.end.x, a.end.y - b.end.y)
      if (g.pinchStart > 0) {
        const ratio = d / g.pinchStart
        if (Math.abs(ratio - 1) >= PINCH_RATIO) {
          // 捏合只缩放本地（不发事件）。一旦进入捏合，这次手势不再当滚动。
          g.pinch = true
          setZoom(clampZoom(g.zoomStart * ratio))
          return
        }
      }
      // 双指上下滑：按累计位移凑整档，40px 一档。
      const y = (a.end.y + b.end.y) / 2
      const dy = y - g.lastScrollY
      g.lastScrollY = y
      g.scrollAccum += dy
      const notches = wheelNotches(g.scrollAccum)
      if (notches !== 0) {
        send({ op: 'wheel', dy: notches })
        g.scrolled = true
        g.scrollAccum -= notches * 40
      }
      return
    }

    // 单指：按住超过阈值前就移动很多 → 取消长按定时器（快速拖动走事后判定）。
    if (g.active.size === 1 && !g.dragging && g.holdTimer) {
      const track = [...g.active.values()][0]
      if (track && Math.hypot(track.end.x - track.start.x, track.end.y - track.start.y) > DRAG_SLOP) {
        window.clearTimeout(g.holdTimer)
        g.holdTimer = 0
      }
    }
  }

  const finishTouch = (e: React.TouchEvent, cancelled = false) => {
    if (readOnly) return
    e.preventDefault()
    const g = gestureRef.current
    for (const t of Array.from(e.changedTouches)) {
      // 系统取消（来电、通知中心下拉）不能当轻点：那会把远端点一下。
      if (cancelled) {
        const track = g.active.get(t.identifier)
        if (track) track.cancelled = true
      }
      g.active.delete(t.identifier)
    }
    if (g.active.size > 0) return // 还有手指在按：等最后一根抬起再判定
    if (g.holdTimer) window.clearTimeout(g.holdTimer)
    g.holdTimer = 0
    if (g.dragging) {
      g.dragging = false
      // 被系统取消时补一个 up，否则远端会一直按着（拖选/拖窗口停不下来）。
      const last = g.last
      if (last) {
        const p = normalizePoint(last.x, last.y, rectOf())
        send({ op: 'up', x: p.x, y: p.y })
      }
      resetGesture()
      return
    }
    applyGesture(Date.now())
  }

  // ── 鼠标（桌面浏览器调试用）────────────────────────────────
  //
  // 移动浏览器在 touchend 后会补发一整套 mouse 事件，不做时间窗过滤的话
  // 一次轻点会变成「click + click」。
  const mouseRef = useRef<{ down: boolean; track: GestureTrack | null }>({ down: false, track: null })

  const onMouseDown = (e: React.MouseEvent) => {
    if (readOnly || Date.now() - touchedAtRef.current < TOUCH_MOUSE_GUARD_MS) return
    e.preventDefault()
    const g = gestureRef.current
    resetGesture()
    const point = { x: e.clientX, y: e.clientY }
    const track: GestureTrack = { start: point, end: point, samples: [point] }
    g.startAt = Date.now()
    g.tracks = [track]
    g.active.set(0, track)
    g.last = point
    mouseRef.current = { down: true, track }
    startHold()
  }

  const onMouseMove = (e: React.MouseEvent) => {
    const m = mouseRef.current
    if (!m.down || !m.track) return
    const point = { x: e.clientX, y: e.clientY }
    m.track.end = point
    if (!m.track.samples) m.track.samples = [m.track.start]
    if (m.track.samples.length < 120) m.track.samples.push(point)
    gestureRef.current.last = point
    if (gestureRef.current.dragging) {
      const p = normalizePoint(point.x, point.y, rectOf())
      send({ op: 'move', x: p.x, y: p.y })
    } else if (gestureRef.current.holdTimer) {
      if (Math.hypot(point.x - m.track.start.x, point.y - m.track.start.y) > DRAG_SLOP) {
        window.clearTimeout(gestureRef.current.holdTimer)
        gestureRef.current.holdTimer = 0
      }
    }
  }

  const onMouseUp = () => {
    const m = mouseRef.current
    if (!m.down) return
    m.down = false
    const g = gestureRef.current
    g.active.delete(0)
    if (g.dragging) {
      g.dragging = false
      const last = g.last
      if (last) {
        const p = normalizePoint(last.x, last.y, rectOf())
        send({ op: 'up', x: p.x, y: p.y })
      }
      resetGesture()
      mouseRef.current = { down: false, track: null }
      return
    }
    applyGesture(Date.now())
    mouseRef.current = { down: false, track: null }
  }

  const onWheel = (e: React.WheelEvent) => {
    if (readOnly) return
    e.preventDefault()
    // 鼠标一格 deltaY≈100，用符号而不是 magnitude：一格就是滚一格。
    send({ op: 'wheel', dy: e.deltaY > 0 ? 1 : -1 })
  }

  // ── 键盘与快捷操作 ──────────────────────────────────────────
  const sendText = () => {
    const text = keyText
    if (!text) return
    sendRaw({ type: 'input', op: 'text', text })
    setKeyText('')
  }

  const sendKey = (key: string) => {
    const payload: Record<string, unknown> = { type: 'input', op: 'key', key }
    if (cmdMod) payload.mods = 'cmd'
    sendRaw(payload)
    if (cmdMod) setCmdMod(false)
  }

  const runAction = async (action: 'screenshot' | 'lock' | 'open_app' | 'open_url') => {
    setActionMsg('')
    setActionPath('')
    const args: Record<string, string> = {}
    if (action === 'open_app') {
      if (!appName.trim()) {
        setActionMsg('先填应用名，例如 Finder / Safari')
        return
      }
      args.name = appName.trim()
    }
    if (action === 'open_url') {
      if (!/^https?:\/\//i.test(url.trim())) {
        setActionMsg('网址要以 http:// 或 https:// 开头')
        return
      }
      args.url = url.trim()
    }
    setActionMsg('执行中…')
    try {
      const res = await remoteApi.action(cur.id, action, args)
      setActionMsg(res.ok ? res.detail || '已执行' : res.detail || '执行失败')
      if (res.path) setActionPath(res.path)
    } catch (e) {
      setActionMsg((e as Error).message)
    }
  }

  const endNow = async () => {
    try {
      await remoteApi.endSession(cur.id)
    } catch {
      // 已经结束/没权限都无所谓：本地照样退出，别把用户困在这一屏。
    }
    onExit('user_ended')
  }

  const stats = serverStats
  const fps = stats?.fps && stats.fps > 0 ? stats.fps : localFps
  const width = stats?.width || frame.w
  const height = stats?.height || frame.h
  const dropped = Math.max(stats?.dropped ?? 0, droppedRef.current)
  // 掉帧率的分母必须和 dropped 来自同一个计数器：服务端给了累计 frames 就用它，
  // 否则本地只收到几帧时会算出「掉帧 93%」这种假警。
  const frames = stats?.frames && stats.frames > 0 ? stats.frames : framesRef.current
  const started = clock - new Date(cur.createdAt).getTime()

  return (
    <div className="remote-viewer">
      <div className="remote-viewer-head">
        <div className="remote-viewer-title">
          <span className="remote-device-name">{cur.device || cur.deviceId}</span>
          <span className={`remote-tag ${sessionStatusTone(cur.status)}`}>
            {sessionStatusLabel(cur.status)}
          </span>
          <span className="remote-tag dim">{modeLabel(cur.mode)}</span>
        </div>
        <button className="btn small" onClick={() => void endNow()}>
          返回
        </button>
      </div>

      {readOnly && <div className="remote-readonly">{readOnlyMsg}</div>}
      {notice && <div className="remote-note">{notice}</div>}
      {err && <div className="remote-note warn">{err}</div>}

      <div className="remote-stage">
        {/* 事件挂在 canvas 上而不是外层：外层在桌面会留出左右的黑边，
            挂在外层的话点黑边也会被钳成 0/1 的坐标，等于误点画面边缘。 */}
        <div className="remote-canvas-wrap">
          <canvas
            ref={canvasRef}
            className="remote-canvas"
            width={640}
            height={400}
            style={{ transform: `scale(${zoom})` }}
            onTouchStart={onTouchStart}
            onTouchMove={onTouchMove}
            onTouchEnd={(e) => finishTouch(e)}
            onTouchCancel={(e) => finishTouch(e, true)}
            onMouseDown={onMouseDown}
            onMouseMove={onMouseMove}
            onMouseUp={onMouseUp}
            onMouseLeave={onMouseUp}
            onWheel={onWheel}
          />
        </div>

        {cur.status === 'pending_approval' && (
          <div className="remote-overlay">
            <div className="remote-overlay-title">等待电脑上确认接管</div>
            <div className="remote-overlay-sub">
              电脑屏幕右下角会弹出确认框。{Math.ceil(waitLeft / 1000)} 秒后自动放弃。
            </div>
            <div className="remote-overlay-sub dim">
              这一步是防误触的：手机上误点「接管」不会立刻把键鼠交出去。
            </div>
          </div>
        )}
        {cur.status === 'active' && frame.w === 0 && !err && (
          <div className="remote-overlay">
            <div className="remote-overlay-title">正在接收画面…</div>
            <div className="remote-overlay-sub">
              {conn === 'retry' ? '连接断开，正在重连（会重新换一次性票据）' : '首帧还没到'}
            </div>
          </div>
        )}
        {isTerminalStatus(cur.status) && (
          <div className="remote-overlay">
            <div className="remote-overlay-title">会话{sessionStatusLabel(cur.status)}</div>
            <div className="remote-overlay-sub">回到列表可以重新发起。</div>
            <button className="btn" onClick={() => onExit(cur.status)}>
              回到设备列表
            </button>
          </div>
        )}
      </div>

      <div className="remote-tools">
        <div className="remote-tier">
          <span className="remote-tools-label">画质</span>
          <div className="seg remote-tier-seg">
            {QUALITY_PRESETS.map((p) => (
              <button
                key={p.key}
                className={`seg-item ${tier === p.key ? 'on' : ''}`}
                onClick={() => {
                  setTier(p.key)
                  tierRef.current = p.key
                  sendQuality()
                }}
                title={`${p.maxWidth}px / ${p.fps}fps / q${p.quality}`}
              >
                {p.label}
              </button>
            ))}
          </div>
          {/* 只读时手势全关，放大只能靠这两个按钮；有手势时它也是「一键复位」 */}
          <div className="remote-zoom">
            <button
              className="btn small"
              onClick={() => setZoom((z) => clampZoom(z - 0.25))}
              title="缩小"
            >
              −
            </button>
            <span className="remote-zoom-val">{Math.round(zoom * 100)}%</span>
            <button
              className="btn small"
              onClick={() => setZoom((z) => clampZoom(z + 0.25))}
              title="放大"
            >
              +
            </button>
          </div>
        </div>

        <div className="remote-actions">
          <button
            className="btn small"
            disabled={!!actionsWhy}
            title={actionsWhy || '截屏存到数据目录'}
            onClick={() => void runAction('screenshot')}
          >
            截屏
          </button>
          <button
            className="btn small"
            disabled={!!actionsWhy}
            title={actionsWhy || '锁屏 / 立即熄屏'}
            onClick={() => void runAction('lock')}
          >
            锁屏
          </button>
          <input
            className="input slim remote-input"
            value={appName}
            onChange={(e) => setAppName(e.target.value)}
            placeholder="应用名"
            aria-label="要打开的应用名"
            spellCheck={false}
          />
          <button
            className="btn small"
            disabled={!!actionsWhy}
            title={actionsWhy || '在电脑上打开这个应用'}
            onClick={() => void runAction('open_app')}
          >
            打开应用
          </button>
        </div>

        <div className="remote-actions">
          <input
            className="input slim remote-input wide"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://…"
            aria-label="要打开的网址"
            spellCheck={false}
          />
          <button
            className="btn small"
            disabled={!!actionsWhy}
            title={actionsWhy || '在电脑浏览器里打开这个网址'}
            onClick={() => void runAction('open_url')}
          >
            打开网址
          </button>
        </div>

        {actionMsg && (
          <div className="remote-action-msg">
            <span>{actionMsg}</span>
            {actionPath && (
              <>
                <code className="mono small">{actionPath}</code>
                <button className="btn small" onClick={() => void copyText(actionPath)}>
                  复制路径
                </button>
              </>
            )}
          </div>
        )}

        {/* 只读时整块不渲染：留一个禁用的键盘会让人一直怀疑「是不是没点上」。 */}
        {!readOnly && (
          <div className="remote-keys">
            <div className="remote-keys-row">
              <button className="btn small" onClick={() => sendKey('Enter')}>
                Enter
              </button>
              <button className="btn small" onClick={() => sendKey('Tab')}>
                Tab
              </button>
              <button className="btn small" onClick={() => sendKey('Escape')}>
                Esc
              </button>
              <button className="btn small" onClick={() => sendKey('Backspace')}>
                ⌫
              </button>
              <button className="btn small" onClick={() => sendKey('ArrowUp')}>
                ↑
              </button>
              <button className="btn small" onClick={() => sendKey('ArrowDown')}>
                ↓
              </button>
              <button className="btn small" onClick={() => sendKey('ArrowLeft')}>
                ←
              </button>
              <button className="btn small" onClick={() => sendKey('ArrowRight')}>
                →
              </button>
              <button
                className={`btn small ${cmdMod ? 'remote-toggle-on' : ''}`}
                aria-pressed={cmdMod}
                title="按下后，下一个按键会带上 cmd"
                onClick={() => setCmdMod((v) => !v)}
              >
                ⌘
              </button>
            </div>
            <div className="remote-keys-row">
              <textarea
                className="remote-text"
                value={keyText}
                onChange={(e) => setKeyText(e.target.value)}
                placeholder="虚拟键盘：输入一段文字，点发送"
                rows={2}
              />
              <button className="btn primary remote-send" onClick={sendText} disabled={!keyText}>
                发送
              </button>
            </div>
          </div>
        )}
      </div>

      {/* 底部状态条：手机上是 fixed（拇指可达），桌面是 sticky。 */}
      <div className="remote-status">
        <div className="remote-status-metrics">
          <span>
            <b>{formatFps(fps)}</b> fps
          </span>
          <span>{resolutionText(width, height)}</span>
          <span className={dropped > 0 ? 'remote-drop' : ''}>
            掉帧 {dropRate(frames, dropped)}
          </span>
          <span className="remote-status-conn">
            {conn === 'live' ? '已连接' : conn === 'retry' ? '重连中…' : conn === 'connecting' ? '连接中…' : '已断开'}
          </span>
          <span className="dim">时长 {formatDuration(started)}</span>
        </div>
        <button className="remote-disconnect" onClick={() => void endNow()}>
          立即断开
        </button>
      </div>
    </div>
  )
}

function clampZoom(z: number): number {
  if (!Number.isFinite(z)) return 1
  return Math.min(3, Math.max(1, Math.round(z * 100) / 100))
}

/** 服务端 stats 字段可能缺，统一补 0，避免界面出现 undefined。 */
function normalizeStats(raw: any): RemoteSession['stats'] {
  const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)
  return {
    fps: n(raw?.fps),
    width: n(raw?.width),
    height: n(raw?.height),
    quality: n(raw?.quality),
    frames: n(raw?.frames),
    dropped: n(raw?.dropped),
    inputs: n(raw?.inputs),
  }
}
