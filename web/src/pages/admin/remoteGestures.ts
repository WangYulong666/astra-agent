// 手势判定：把「一次触摸序列」映射成远程输入事件。
//
// 判定逻辑必须可测，所以这里不碰 DOM：调用方把每个手指的轨迹（起点/终点/
// 采样点）与时间喂进来，这里只回答「这是什么手势、要发哪些事件」。
// 组件负责采集轨迹、以及长按生效后**实时**发 down/move/up。
//
// 映射表（照 WorkBuddy 那张）：
//   单指轻点 → click          单指双击 → dblclick
//   单指长按 600ms 后拖动 → down → move×N → up
//   双指轻点 → rightclick     双指上下滑 → wheel（40px 一档）
//   双指捏合 → 只缩放本地画面，**不发任何事件**

import { normalizePoint } from './remoteModel'
import type { NormPoint, RectLike } from './remoteModel'

export type Point = { x: number; y: number }

/** 一根手指的轨迹。坐标是浏览器 client 坐标（CSS 像素，含页面偏移）。 */
export type GestureTrack = {
  start: Point
  end: Point
  /** 采样点（含起点与终点）。缺省时按两点直线处理。 */
  samples?: Point[]
  /** 这一指最终是正常抬起还是被系统取消（来电、通知中心下拉）。 */
  cancelled?: boolean
}

export type GestureInput = {
  /** 参与这次手势的手指，顺序无所谓；只取前两指判定双指手势。 */
  tracks: GestureTrack[]
  /** 从第一指按下到最后一指抬起的时长。 */
  durationMs: number
  /** 手势结束的绝对时刻（毫秒时间戳），用来判双击间隔。 */
  endAt: number
  /** 画面在屏幕上的矩形（getBoundingClientRect）。 */
  rect: RectLike
  /** 上一次轻点的时刻与位置；没有就传 null。 */
  lastTap?: { at: number; point: Point } | null
}

export type RemoteInputEvent =
  | { op: 'click' | 'dblclick' | 'rightclick' | 'down' | 'up' | 'move'; x: number; y: number }
  | { op: 'wheel'; dy: number }

export type GestureKind =
  | 'tap'
  | 'double-tap'
  | 'two-finger-tap'
  | 'drag'
  | 'scroll'
  | 'pinch'
  | 'none'

export type GestureResult = {
  kind: GestureKind
  events: RemoteInputEvent[]
  /** pinch 时的缩放比（相对手势开始），其它手势为 1。 */
  scale: number
}

// ── 阈值 ────────────────────────────────────────────────────────
//
// 这些数字都按「手指在玻璃上的实际误差」定，不是拍脑袋：
// TAP_SLOP 12px 大于安卓的 8dp 触摸抖动、小于一个图标；
// DOUBLE_TAP_MS 320ms 与系统双击间隔（300–350ms）同量级；
// WHEEL_STEP 40px 是「两指滑一小段」能跨过的最小格数，再小会连发滚轮。

export const TAP_MAX_MS = 300
export const TAP_SLOP = 12
export const DOUBLE_TAP_MS = 320
export const DOUBLE_TAP_SLOP = 40
export const LONG_PRESS_MS = 600
export const DRAG_SLOP = 24
export const WHEEL_STEP = 40
export const PINCH_RATIO = 0.08
export const TWO_FINGER_SLOP = 16

function dist(a: Point, b: Point): number {
  return Math.hypot(a.x - b.x, a.y - b.y)
}

function mid(a: Point, b: Point): Point {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 }
}

/** 轨迹长度（累计位移，不是首尾直线距离）：走一个来回也算「动过」。 */
function pathLength(points: Point[]): number {
  let sum = 0
  for (let i = 1; i < points.length; i++) sum += dist(points[i - 1], points[i])
  return sum
}

/** 双指滑动 → 滚轮档数。向下为正、向上为负，不足一档返回 0。 */
export function wheelNotches(dyPx: number): number {
  if (!Number.isFinite(dyPx)) return 0
  return Math.trunc(dyPx / WHEEL_STEP)
}

/** 捏合比：两指间距的变化比例。起始间距为 0 时按 1 处理（没意义）。 */
export function pinchRatio(startDist: number, endDist: number): number {
  if (!(startDist > 0) || !Number.isFinite(endDist)) return 1
  return endDist / startDist
}

function trackPoints(track: GestureTrack): Point[] {
  const samples = track.samples && track.samples.length > 0 ? track.samples : [track.start, track.end]
  // 采样点里最后一点可能落后于实际的 end（抬手那一帧没有 move 事件），补上。
  const last = samples[samples.length - 1]
  if (last.x !== track.end.x || last.y !== track.end.y) return [...samples, track.end]
  return samples
}

function norm(point: Point, rect: RectLike): NormPoint {
  return normalizePoint(point.x, point.y, rect)
}

/**
 * 判定一次已完成的手势。
 *
 * 注意：长按拖动由组件在按住 600ms 后**实时**发 down/move/up（见 RemoteViewer），
 * 这里返回的 drag 事件用于「手势已结束后回放一遍」（例如快速拖动、
 * 或者在 touchend 里补发）。两条路径共用同一套事件形状，不会出现
 * 「实时拖动和事后拖动发的东西不一样」。
 */
export function detectGesture(input: GestureInput): GestureResult {
  const tracks = (input.tracks || []).filter(Boolean)
  if (tracks.length >= 2) return detectTwoFinger(tracks.slice(0, 2), input)
  if (tracks.length === 1) return detectOneFinger(tracks[0], input)
  return { kind: 'none', events: [], scale: 1 }
}

function detectTwoFinger(tracks: GestureTrack[], input: GestureInput): GestureResult {
  const [a, b] = tracks
  const startMid = mid(a.start, b.start)
  const endMid = mid(a.end, b.end)
  const moved = dist(startMid, endMid)
  const ratio = pinchRatio(dist(a.start, b.start), dist(a.end, b.end))
  const dy = endMid.y - startMid.y
  const notches = wheelNotches(dy)

  // 顺序有讲究：先把「一档都没滑满」的情况留给捏合/轻点，
  // 否则双指滑动过程中必然出现的那点间距变化会被误判成捏合。
  if (notches !== 0) {
    return { kind: 'scroll', events: [{ op: 'wheel', dy: notches }], scale: ratio }
  }
  if (Math.abs(ratio - 1) >= PINCH_RATIO) {
    // 捏合只改本地缩放，绝不发事件——发了就等于在远端按住两处拖，
    // 桌面上的窗口会被拖歪。
    return { kind: 'pinch', events: [], scale: ratio }
  }
  if (input.durationMs <= TAP_MAX_MS && moved <= TWO_FINGER_SLOP) {
    const p = norm(endMid, input.rect)
    return { kind: 'two-finger-tap', events: [{ op: 'rightclick', x: p.x, y: p.y }], scale: 1 }
  }
  return { kind: 'none', events: [], scale: ratio }
}

function detectOneFinger(track: GestureTrack, input: GestureInput): GestureResult {
  const points = trackPoints(track)
  const travelled = pathLength(points)
  const direct = dist(track.start, track.end)

  if (
    !track.cancelled &&
    input.durationMs <= TAP_MAX_MS &&
    direct <= TAP_SLOP &&
    travelled <= TAP_SLOP
  ) {
    const p = norm(track.start, input.rect)
    const last = input.lastTap
    if (
      last &&
      input.endAt - last.at <= DOUBLE_TAP_MS &&
      dist(last.point, track.start) <= DOUBLE_TAP_SLOP
    ) {
      return { kind: 'double-tap', events: [{ op: 'dblclick', x: p.x, y: p.y }], scale: 1 }
    }
    return { kind: 'tap', events: [{ op: 'click', x: p.x, y: p.y }], scale: 1 }
  }

  // 长按（≥600ms）一律按拖动处理：即使没移动，down+up 在远端也等价于在这点按下，
  // 这正是「长按选中/右键菜单」需要的行为。快速拖动（<600ms 但移动够远）
  // 也按拖动——鼠标语义就是按下-移动-抬起，让人快速划一下却没反应更奇怪。
  if (input.durationMs >= LONG_PRESS_MS || travelled >= DRAG_SLOP) {
    const first = points[0]
    const last = points[points.length - 1]
    const events: RemoteInputEvent[] = []
    const p0 = norm(first, input.rect)
    events.push({ op: 'down', x: p0.x, y: p0.y })
    // 中间点：跳过首尾，避免同一点连发两个 move。
    for (let i = 1; i < points.length - 1; i++) {
      const p = norm(points[i], input.rect)
      events.push({ op: 'move', x: p.x, y: p.y })
    }
    const p1 = norm(last, input.rect)
    events.push({ op: 'up', x: p1.x, y: p1.y })
    return { kind: 'drag', events, scale: 1 }
  }

  return { kind: 'none', events: [], scale: 1 }
}
