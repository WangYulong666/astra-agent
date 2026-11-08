import { describe, expect, it } from 'vitest'
import type { Capability, Skill } from '../../api'
import {
  countCapabilities,
  emptyText,
  filterCapabilities,
  GUIDES,
  insightStatus,
  isUnavailable,
  kindLabel,
  skillStatus,
  truncate,
} from './loopModel'

// 构造器：只填展示/筛选逻辑会读的字段。
function cap(
  name: string,
  kind: 'api' | 'cli' | 'skill',
  enabled: boolean,
  extra: Partial<Capability> = {},
): Capability {
  return { name, kind, enabled, description: `${name} 描述`, ...extra }
}

function skill(status: 'ready' | 'unavailable'): Skill {
  return { name: 's', description: 'd', status }
}

describe('映射：kind / skill.status / insight.status', () => {
  it('三种 kind 给标签，ready 绿、unavailable 黄', () => {
    expect(kindLabel('api')).toBe('API')
    expect(kindLabel('cli')).toBe('CLI')
    expect(kindLabel('skill')).toBe('Skill')
    expect(kindLabel('weird')).toBe('weird')
    expect(skillStatus(skill('ready'))).toEqual({ label: 'ready', tone: 'ready' })
    expect(skillStatus(skill('unavailable')).tone).toBe('warn')
  })

  it('诊断状态分类，未知值退到中性不标红', () => {
    expect(insightStatus('failed').tone).toBe('failed')
    expect(insightStatus('cancelled').label).toBe('已停止')
    expect(insightStatus('completed').tone).toBe('ok')
    expect(insightStatus('new_status').tone).toBe('dim')
  })
})

describe('计数：三类 kind + 启用/未启用', () => {
  it('基于全量清单统计，不受筛选影响', () => {
    const items = [
      cap('a', 'api', true),
      cap('b', 'api', false),
      cap('c', 'cli', true),
      cap('d', 'skill', false),
      cap('e', 'skill', true),
    ]
    expect(countCapabilities(items)).toEqual({
      api: 2,
      cli: 1,
      skill: 2,
      enabled: 3,
      disabled: 2,
    })
    expect(countCapabilities(null)).toEqual({
      api: 0,
      cli: 0,
      skill: 0,
      enabled: 0,
      disabled: 0,
    })
  })
})

describe('筛选：kind + 只看已启用 + 关键字', () => {
  const items = [
    cap('memory.search', 'api', true, { endpoint: '/v1/memory/search' }),
    cap('astra-cli whoami', 'cli', false, { requires: ['astra-cli'] }),
    cap('astra-memory', 'skill', true, { requires: ['astra-cli'] }),
  ]

  it('按 kind 与只看已启用分别收窄', () => {
    expect(filterCapabilities(items, { kind: 'skill', onlyEnabled: false, query: '' })).toHaveLength(1)
    expect(filterCapabilities(items, { kind: 'all', onlyEnabled: true, query: '' })).toHaveLength(2)
    expect(
      filterCapabilities(items, { kind: 'cli', onlyEnabled: true, query: '' }),
    ).toHaveLength(0)
  })

  it('关键字命中 name/description/endpoint/requires，大小写不敏感', () => {
    expect(
      filterCapabilities(items, { kind: 'all', onlyEnabled: false, query: 'MEMORY' }),
    ).toHaveLength(2)
    expect(
      filterCapabilities(items, { kind: 'all', onlyEnabled: false, query: 'whoami' }),
    ).toHaveLength(1)
    expect(
      filterCapabilities(items, { kind: 'all', onlyEnabled: false, query: '/v1/memory' }),
    ).toHaveLength(1)
    expect(
      filterCapabilities(items, { kind: 'all', onlyEnabled: false, query: '不存在' }),
    ).toHaveLength(0)
  })
})

describe('空态文案', () => {
  it('失败诊断空态明确说「还没有失败诊断」而不是空白', () => {
    expect(emptyText('insights', false)).toContain('还没有失败诊断')
    expect(emptyText('insights', true)).toContain('session_id')
    expect(emptyText('capabilities', false)).toContain('还没有条目')
    expect(emptyText('skills', true)).toContain('筛选条件')
  })
})

describe('503 引导', () => {
  it('503 及等价「拿不到清单」状态判为未启用，4xx 真实错误不算', () => {
    expect(isUnavailable({ status: 503 })).toBe(true)
    expect(isUnavailable({ status: 404 })).toBe(true)
    expect(isUnavailable({ status: 502 })).toBe(true)
    expect(isUnavailable({ status: 0 })).toBe(true)
    expect(isUnavailable({ status: 401 })).toBe(false)
    expect(isUnavailable({ status: 403 })).toBe(false)
    expect(isUnavailable(new Error('boom'))).toBe(false)
  })

  it('三页引导都带「未启用」标题与逐条配置说明', () => {
    for (const page of ['skills', 'capabilities', 'insights'] as const) {
      expect(GUIDES[page].title).toContain('未启用')
      expect(GUIDES[page].rows.length).toBeGreaterThan(0)
      expect(GUIDES[page].note.length).toBeGreaterThan(0)
    }
    expect(GUIDES.insights.rows.map((r) => r.k + r.v).join(' ')).toContain('INSIGHT_REPLAY')
  })

  it('错误单元格截断但保留完整信息的入口', () => {
    expect(truncate('短错误')).toBe('短错误')
    expect(truncate('x'.repeat(80), 20)).toHaveLength(21) // 20 字符 + …
    expect(truncate(undefined)).toBe('')
  })
})
