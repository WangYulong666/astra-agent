import { describe, expect, it } from 'vitest'
import type { ActivityEntry } from '../../api'
import {
  countStatuses,
  emptyHint,
  kindMeta,
  statusMeta,
} from './activityModel'

// 条目构造器：只填展示逻辑会读的字段。
function entry(kind: string, status: string, title: string, time: string): ActivityEntry {
  return { kind, status, title, time }
}

describe('映射：kind / status → 图标、标签、色调', () => {
  it('八个 kind 都给出中文标签与单字图标，status 给出徽标', () => {
    expect(kindMeta('plan').label).toBe('任务')
    expect(kindMeta('tool').glyph).toBe('工')
    expect(kindMeta('approval').label).toBe('审批')
    expect(kindMeta('egress').label).toBe('出网')
    expect(kindMeta('message').label).toBe('回复')
    expect(kindMeta('child').label).toBe('子任务')
    expect(kindMeta('state').label).toBe('状态')
    expect(kindMeta('error').label).toBe('错误')

    expect(statusMeta('done').label).toBe('已完成')
    expect(statusMeta('blocked').tone).toBe('blocked')
    expect(statusMeta('failed').label).toBe('失败')
  })

  it('未知 kind / status 退到中性样式，不冒充错误', () => {
    expect(kindMeta('something_new').tone).toBe('state')
    expect(kindMeta('something_new').label).toBe('something_new')
    expect(kindMeta('').label).toBe('事件')
    expect(statusMeta('').tone).toBe('running')
    expect(statusMeta('weird').label).toBe('weird')
  })
})

describe('计数：四状态统计 + 现在卡在哪', () => {
  it('分类计数，Current 取时间最靠后的一条 blocked 的标题', () => {
    const entries: ActivityEntry[] = [
      entry('plan', 'done', '开始', '2026-01-01T00:00:00Z'),
      entry('tool', 'done', '跑命令', '2026-01-01T00:01:00Z'),
      entry('state', 'blocked', '等待审批', '2026-01-01T00:02:00Z'),
      entry('approval', 'blocked', '删除文件', '2026-01-01T00:05:00Z'), // 最新的 blocked
      entry('error', 'failed', '出错了', '2026-01-01T00:03:00Z'),
      entry('tool', 'running', '进行中的工具', '2026-01-01T00:04:00Z'),
      entry('weird', 'unknown', '未知', '2026-01-01T00:06:00Z'),
    ]
    const c = countStatuses(entries)
    expect(c.done).toBe(2)
    expect(c.running).toBe(1)
    expect(c.blocked).toBe(2)
    expect(c.failed).toBe(1)
    expect(c.current).toBe('删除文件')
  })

  it('没有阻塞项时 Current 为空串；空列表全为 0', () => {
    const c = countStatuses([entry('message', 'done', '答', '2026-01-01T00:00:00Z')])
    expect(c.done).toBe(1)
    expect(c.current).toBe('')
    const zero = countStatuses([])
    expect(zero).toEqual({ done: 0, running: 0, blocked: 0, failed: 0, current: '' })
  })
})

describe('空态：没输入 / 未启用 / 空数据', () => {
  it('默认空时提示「输入 run id 查看」，未启用时提示「能力未启用」', () => {
    expect(emptyHint('no-id', 'run')).toContain('输入 run id 查看')
    expect(emptyHint('no-id', 'session')).toContain('输入 session id 查看')
    expect(emptyHint('disabled', 'run')).toBe('能力未启用')
    expect(emptyHint('no-entries', 'run')).toBe('这个 id 下还没有行动记录')
    expect(emptyHint('error', 'run')).toBe('加载失败，请重试')
  })
})
