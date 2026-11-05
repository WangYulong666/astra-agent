import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  clockTime,
  fmtNumber,
  fmtTime,
  fmtUsd,
  humanSize,
  isZeroTime,
  relTime,
  shortId,
} from './format'

// 本地时区无关：用本地构造 + 本地格式化，两边时区相同，断言才不会随 CI 机器漂移。

describe('fmtNumber：token 千分位', () => {
  it('几十万的量必须分组，否则读不出量级', () => {
    expect(fmtNumber(1234567)).toBe('1,234,567')
    expect(fmtNumber(1000)).toBe('1,000')
  })

  it('空值与 NaN 都落到 "0" 而不是 "NaN"', () => {
    expect(fmtNumber(0)).toBe('0')
    expect(fmtNumber(undefined)).toBe('0')
    expect(fmtNumber(Number.NaN)).toBe('0')
    // 后端某些字段可能给 null（omitempty 对指针才生效），实际代码也按 0 处理。
    expect(fmtNumber(null as unknown as number)).toBe('0')
  })
})

describe('fmtUsd：微美元 → 4 位小数', () => {
  it('单次调用常在 $0.001 量级，必须保留 4 位，否则整页都是 0.00', () => {
    expect(fmtUsd(1234)).toBe('$0.0012')
    expect(fmtUsd(250000)).toBe('$0.2500')
  })

  it('0 与空值都显示 $0.0000', () => {
    expect(fmtUsd(0)).toBe('$0.0000')
    expect(fmtUsd(undefined)).toBe('$0.0000')
  })
})

describe('时间格式化', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('relTime 按分钟/小时/天分档', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 0, 10, 12, 0, 0))
    const now = Date.now()
    const ago = (ms: number) => new Date(now - ms).toISOString()

    expect(relTime(ago(30 * 1000))).toBe('刚刚')
    expect(relTime(ago(5 * 60 * 1000))).toBe('5 分钟前')
    expect(relTime(ago(3 * 60 * 60 * 1000))).toBe('3 小时前')
    expect(relTime(ago(2 * 24 * 60 * 60 * 1000))).toBe('2 天前')
  })

  it('relTime 超过 7 天改显示日期，空值与非法值返回空串', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 0, 20, 12, 0, 0))
    const old = new Date(2026, 0, 2, 9, 0, 0)
    // 具体 locale 分隔符（1/2 或 01/02）随 Node ICU 版本变，只锁定「月/日」结构。
    expect(relTime(old.toISOString())).toMatch(/^\d{1,2}\/\d{1,2}$/)

    expect(relTime('')).toBe('')
    expect(relTime('not-a-timestamp')).toBe('')
  })

  it('clockTime 输出 HH:mm，非法值返回空串', () => {
    expect(clockTime(new Date(2026, 0, 2, 14, 5).toISOString())).toBe('14:05')
    expect(clockTime('')).toBe('')
    expect(clockTime('2026-13-45')).toBe('')
  })

  it('isZeroTime 认出 Go 的 time.Time 零值（omitempty 不生效的那个坑）', () => {
    expect(isZeroTime('0001-01-01T00:00:00Z')).toBe(true)
    expect(isZeroTime(undefined)).toBe(true)
    expect(isZeroTime('')).toBe(true)
    expect(isZeroTime('2026-01-02T03:04:05Z')).toBe(false)
  })

  it('fmtTime 对零值/非法值统一显示「—」，有效值带年月日与时分', () => {
    expect(fmtTime(undefined)).toBe('—')
    expect(fmtTime('0001-01-01T00:00:00Z')).toBe('—')
    expect(fmtTime('garbage')).toBe('—')

    const s = fmtTime(new Date(2026, 0, 2, 14, 5).toISOString())
    expect(s).toMatch(/2026\/0?1\/0?2/)
    expect(s).toMatch(/14:05/)
  })
})

describe('humanSize / shortId', () => {
  it('按 B/KB/MB 分档，边界值不串档', () => {
    expect(humanSize(0)).toBe('0 B')
    expect(humanSize(1023)).toBe('1023 B')
    expect(humanSize(1024)).toBe('1.0 KB')
    expect(humanSize(1024 * 1024 - 1)).toBe('1024.0 KB')
    expect(humanSize(1024 * 1024)).toBe('1.0 MB')
    expect(humanSize(5.5 * 1024 * 1024)).toBe('5.5 MB')
  })

  it('长 id 截成「前 6 … 后 4」，短 id 原样返回', () => {
    expect(shortId('1234567890')).toBe('1234567890')
    expect(shortId('12345678901')).toBe('123456…8901')
  })
})
