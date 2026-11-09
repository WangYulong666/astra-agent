// 手势判定：一次触摸序列 → 一组远程输入事件。
//
// 表格（照 WorkBuddy 的映射）：
//   单指轻点 → click        单指双击 → dblclick
//   长按 600ms 后拖动 → down/move…/up
//   双指轻点 → rightclick   双指上下滑 → wheel
//   双指捏合 → 只有本地缩放，**不发事件**
//
// 这里不碰 DOM：轨迹与时间都是参数，所以阈值可以直接用数字断言。

import { describe, expect, it } from 'vitest'
import {
  LONG_PRESS_MS,
  WHEEL_STEP,
  detectGesture,
  pinchRatio,
  wheelNotches,
} from './remoteGestures'
import type { GestureTrack } from './remoteGestures'
import type { RectLike } from './remoteModel'

// 400×200 的画面，铺满一个 400×200 的区域：坐标换算后正好是「像素/400、像素/200」。
const RECT: RectLike = { left: 0, top: 0, width: 400, height: 200 }

function track(samples: [number, number][], cancelled = false): GestureTrack {
  const first = samples[0]
  const last = samples[samples.length - 1]
  return {
    start: { x: first[0], y: first[1] },
    end: { x: last[0], y: last[1] },
    samples: samples.map(([x, y]) => ({ x, y })),
    cancelled,
  }
}

describe('单指手势', () => {
  it('轻点 → click，坐标归一化到 0..1', () => {
    const res = detectGesture({
      tracks: [track([[200, 100]])],
      durationMs: 120,
      endAt: 10_000,
      rect: RECT,
    })
    expect(res.kind).toBe('tap')
    expect(res.events).toEqual([{ op: 'click', x: 0.5, y: 0.5 }])
  })

  it('紧跟在上一次轻点之后（≤320ms、位置相近）→ dblclick', () => {
    const res = detectGesture({
      tracks: [track([[200, 100]])],
      durationMs: 110,
      endAt: 10_000,
      rect: RECT,
      lastTap: { at: 9800, point: { x: 202, y: 98 } },
    })
    expect(res.kind).toBe('double-tap')
    expect(res.events).toEqual([{ op: 'dblclick', x: 0.5, y: 0.5 }])
  })

  it('两次轻点间隔太久（>320ms）→ 还是 click，不误判双击', () => {
    const res = detectGesture({
      tracks: [track([[200, 100]])],
      durationMs: 110,
      endAt: 10_000,
      rect: RECT,
      lastTap: { at: 9000, point: { x: 200, y: 100 } },
    })
    expect(res.kind).toBe('tap')
  })

  it('两次轻点位置相距很远 → 不是双击（防「点了左上又点右下」变成双击）', () => {
    const res = detectGesture({
      tracks: [track([[40, 20]])],
      durationMs: 100,
      endAt: 10_000,
      rect: RECT,
      lastTap: { at: 9_900, point: { x: 380, y: 180 } },
    })
    expect(res.kind).toBe('tap')
  })

  it('长按 600ms 后拖动 → down → move×N → up，中间点逐个带坐标', () => {
    const res = detectGesture({
      tracks: [track([[0, 0], [100, 50], [200, 100]])],
      durationMs: LONG_PRESS_MS + 200,
      endAt: 20_000,
      rect: RECT,
    })
    expect(res.kind).toBe('drag')
    expect(res.events.map((e) => e.op)).toEqual(['down', 'move', 'up'])
    expect(res.events[0]).toMatchObject({ x: 0, y: 0 })
    expect(res.events[1]).toMatchObject({ x: 0.25, y: 0.25 })
    expect(res.events[2]).toMatchObject({ x: 0.5, y: 0.5 })
  })

  it('快速拖动（<600ms 但移动超过 24px）也按拖动处理', () => {
    const res = detectGesture({
      tracks: [track([[0, 0], [300, 150]])],
      durationMs: 240,
      endAt: 30_000,
      rect: RECT,
    })
    expect(res.kind).toBe('drag')
    expect(res.events.map((e) => e.op)).toEqual(['down', 'up'])
  })

  it('系统取消的触摸不产生点击（否则来电会把远端点一下）', () => {
    const res = detectGesture({
      tracks: [track([[100, 50]], true)],
      durationMs: 80,
      endAt: 31_000,
      rect: RECT,
    })
    expect(res.kind).toBe('none')
    expect(res.events).toEqual([])
  })
})

describe('双指手势', () => {
  it('双指轻点 → rightclick，坐标取两指中点', () => {
    const res = detectGesture({
      tracks: [track([[100, 100]]), track([[300, 100]])],
      durationMs: 150,
      endAt: 40_000,
      rect: RECT,
    })
    expect(res.kind).toBe('two-finger-tap')
    expect(res.events).toEqual([{ op: 'rightclick', x: 0.5, y: 0.5 }])
  })

  it('双指下滑 80px → wheel dy=2（40px 一档）', () => {
    const res = detectGesture({
      tracks: [
        track([[100, 40], [100, 120]]),
        track([[300, 40], [300, 120]]),
      ],
      durationMs: 300,
      endAt: 41_000,
      rect: RECT,
    })
    expect(res.kind).toBe('scroll')
    expect(res.events).toEqual([{ op: 'wheel', dy: 2 }])
  })

  it('双指捏合 → 只给缩放比，不发任何输入事件', () => {
    const res = detectGesture({
      tracks: [track([[100, 100], [60, 100]]), track([[300, 100], [340, 100]])],
      durationMs: 400,
      endAt: 42_000,
      rect: RECT,
    })
    expect(res.kind).toBe('pinch')
    expect(res.events).toEqual([])
    expect(res.scale).toBeGreaterThan(1)
  })

  it('双指滑了不到一档、间距只变一点 → 既不滚也不右键', () => {
    const res = detectGesture({
      tracks: [
        track([[100, 100], [104, 130]]),
        track([[300, 100], [296, 130]]),
      ],
      durationMs: 480,
      endAt: 43_000,
      rect: RECT,
    })
    expect(res.kind).toBe('none')
    expect(res.events).toEqual([])
  })

  it('两只手指数量的边界：轨迹不足时给 none，不崩', () => {
    const res = detectGesture({ tracks: [], durationMs: 10, endAt: 44_000, rect: RECT })
    expect(res.kind).toBe('none')
  })
})

describe('小工具', () => {
  it('wheelNotches 按 40px 一档取整（含方向）', () => {
    expect(WHEEL_STEP).toBe(40)
    expect(wheelNotches(0)).toBe(0)
    expect(wheelNotches(39)).toBe(0)
    expect(wheelNotches(40)).toBe(1)
    expect(wheelNotches(120)).toBe(3)
    expect(wheelNotches(-79)).toBe(-1)
  })

  it('pinchRatio 起始间距为 0 时回 1（不会变成 Infinity）', () => {
    expect(pinchRatio(0, 100)).toBe(1)
    expect(pinchRatio(100, 150)).toBe(1.5)
  })
})
