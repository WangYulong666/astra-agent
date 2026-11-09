// 远程桌面的纯逻辑断言：坐标、画质、状态文案、退避、能力灯。
//
// 这些函数错了界面上照样「有东西」，只有上手用才发现不对（点击偏几像素、
// 重连等 16 秒、缺权限却显示可用）。所以逐条钉住。

import { describe, expect, it } from 'vitest'
import {
  DEFAULT_TIER,
  QUALITY_PRESETS,
  capabilityLamps,
  dropRate,
  formatCountdown,
  formatDuration,
  formatFps,
  isTerminalStatus,
  modeLabel,
  normalizePoint,
  qualityMessage,
  readOnlyReason,
  reconnectDelay,
  resolutionText,
  sessionStatusLabel,
  sessionStatusTone,
} from './remoteModel'

describe('坐标归一化', () => {
  it('带页面偏移：只减去 rect 的 left/top，再除以 CSS 尺寸', () => {
    const p = normalizePoint(300, 200, { left: 100, top: 50, width: 400, height: 300 })
    expect(p).toEqual({ x: 0.5, y: 0.5 })
  })

  it('CSS 缩放：canvas 内部 1600×900、显示 393×221，点正中仍是 0.5', () => {
    // 关键点：归一化必须用 getBoundingClientRect 的尺寸，不能用 canvas.width。
    // 用内部尺寸算的话，同一个触摸点会变成 0.12 —— 点击整体偏到左上角。
    const p = normalizePoint(196.5, 110.5, { left: 0, top: 0, width: 393, height: 221 })
    expect(p).toEqual({ x: 0.5, y: 0.5 })
  })

  it('越界钳到 0..1：手指滑出画面时不产生负坐标', () => {
    const p = normalizePoint(-40, 400, { left: 10, top: 0, width: 200, height: 200 })
    expect(p).toEqual({ x: 0, y: 1 })
  })

  it('零尺寸不除零（首帧没到时 rect 可能是 0）', () => {
    expect(normalizePoint(10, 10, { left: 0, top: 0, width: 0, height: 0 })).toEqual({
      x: 0,
      y: 0,
    })
  })
})

describe('画质档位', () => {
  it('三档参数与后端 Policy 对齐', () => {
    expect(QUALITY_PRESETS.map((p) => [p.key, p.maxWidth, p.fps, p.quality])).toEqual([
      ['low', 640, 6, 45],
      ['medium', 960, 8, 60],
      ['high', 1600, 12, 75],
    ])
    expect(DEFAULT_TIER).toBe('medium')
  })

  it('发出去的 quality 消息字段名与后端 Quality 一致（maxWidth 驼峰）', () => {
    expect(qualityMessage('high')).toEqual({
      type: 'quality',
      maxWidth: 1600,
      fps: 12,
      quality: 75,
    })
  })
})

describe('会话状态文案', () => {
  it('五种状态各自有中文标签与色调', () => {
    const all = [
      ['pending_approval', '等待电脑确认', 'wait'],
      ['active', '进行中', 'ok'],
      ['ended', '已结束', 'off'],
      ['denied', '已被拒绝', 'off'],
      ['expired', '已过期', 'off'],
    ] as const
    for (const [key, label, tone] of all) {
      expect(sessionStatusLabel(key)).toBe(label)
      expect(sessionStatusTone(key)).toBe(tone)
    }
  })

  it('未知状态原样显示，不猜（后端加了新状态时宁可见到英文）', () => {
    expect(sessionStatusLabel('revoked_midway')).toBe('revoked_midway')
    expect(sessionStatusTone('whatever')).toBe('off')
  })

  it('终态判定：只有 ended/denied/expired 会停止轮询', () => {
    expect(isTerminalStatus('ended')).toBe(true)
    expect(isTerminalStatus('denied')).toBe(true)
    expect(isTerminalStatus('expired')).toBe(true)
    expect(isTerminalStatus('active')).toBe(false)
    expect(isTerminalStatus('pending_approval')).toBe(false)
  })

  it('模式文案：view=只看、control=接管', () => {
    expect(modeLabel('view')).toBe('只看')
    expect(modeLabel('control')).toBe('接管')
  })
})

describe('重连退避', () => {
  it('序列是 1s/2s/4s/8s/16s，第 6 次返回 null（不再重试）', () => {
    expect([0, 1, 2, 3, 4].map((i) => reconnectDelay(i))).toEqual([
      1000, 2000, 4000, 8000, 16000,
    ])
    expect(reconnectDelay(5)).toBeNull()
    expect(reconnectDelay(-1)).toBeNull()
  })
})

describe('展示用的格式化', () => {
  it('时长：分秒 / 时分秒，非法值给 —', () => {
    expect(formatDuration(0)).toBe('0:00')
    expect(formatDuration(65_000)).toBe('1:05')
    expect(formatDuration(3_725_000)).toBe('1:02:05')
    expect(formatDuration(-1)).toBe('—')
  })

  it('配对倒计时向上取整（剩 0.2 秒也显示 0:01）', () => {
    expect(formatCountdown(119_000)).toBe('1:59')
    expect(formatCountdown(200)).toBe('0:01')
    expect(formatCountdown(-5)).toBe('0:00')
  })

  it('掉帧率分母是 frames+dropped，没帧时给 —', () => {
    expect(dropRate(0, 0)).toBe('—')
    expect(dropRate(90, 10)).toBe('10%')
    expect(dropRate(995, 5)).toBe('0.5%')
  })

  it('fps 与分辨率：没有数据时给 —，不留 0', () => {
    expect(formatFps(0)).toBe('—')
    expect(formatFps(7.94)).toBe('7.9')
    expect(resolutionText(0, 0)).toBe('—')
    expect(resolutionText(960, 540)).toBe('960×540')
  })
})

describe('能力分层与只读判定', () => {
  const cap = {
    platform: 'darwin',
    capture: { available: true, backend: 'screencapture' },
    actions: { available: true },
    input: {
      available: false,
      reason: '辅助功能权限缺失（System Events -10004）',
      hint: '系统设置 → 隐私与安全 → 辅助功能',
    },
    screen: { width: 1920, height: 1080 },
    maxFps: 12,
  }

  it('三盏灯按层给状态，不可用时带上原因', () => {
    const lamps = capabilityLamps(cap)
    expect(lamps.map((l) => [l.key, l.on])).toEqual([
      ['capture', true],
      ['actions', true],
      ['input', false],
    ])
    expect(lamps[2].reason).toContain('辅助功能')
    expect(lamps[2].hint).toContain('隐私与安全')
  })

  it('没有能力数据时三盏灯全灭，但文案说清是「没拿到」而不是「不支持」', () => {
    const lamps = capabilityLamps(null)
    expect(lamps.every((l) => !l.on)).toBe(true)
    expect(lamps[0].reason).toContain('没有返回')
  })

  it('只读原因分三种：只看模式 / 缺权限 / 状态未知', () => {
    expect(readOnlyReason(cap.input, 'view')).toContain('只看')
    expect(readOnlyReason(cap.input, 'control')).toContain('当前只能观看')
    expect(readOnlyReason(null, 'control')).toContain('没拿到')
    expect(readOnlyReason({ available: true }, 'control')).toBe('')
  })
})
