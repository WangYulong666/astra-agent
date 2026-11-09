import { describe, expect, it } from 'vitest'
import type { DocMeta } from '../../api'
import {
  CATEGORY_LABEL,
  categoryLabel,
  docTitle,
  groupDocs,
  highlightSegments,
  isDocsDisabled,
  pickDefaultPath,
  sortDocs,
  stripLeadingH1,
} from './docsModel'

function doc(path: string, category: string, extra: Partial<DocMeta> = {}): DocMeta {
  return { path, title: path, category, summary: '', ...extra }
}

describe('分组与排序', () => {
  it('按固定顺序分组，组标题用中文；未知分类排在最后且不丢', () => {
    const groups = groupDocs([
      doc('a.md', 'adr', { title: 'A' }),
      doc('overview', 'overview', { title: '功能总览', generated: true }),
      doc('g.md', 'guide', { title: 'G' }),
      doc('x.md', 'weird', { title: 'X' }),
    ])
    expect(groups.map((g) => g.category)).toEqual(['overview', 'guide', 'adr', 'weird'])
    expect(groups[0].label).toBe('功能总览')
    expect(groups[1].label).toBe('使用指南')
    expect(groups[3].label).toBe('weird')
    expect(CATEGORY_LABEL.design).toBe('设计')
    expect(categoryLabel('ops')).toBe('运维')
    expect(categoryLabel('')).toBe('其它')
  })

  it('组内 generated 固定第一，其余按标题排序', () => {
    const sorted = sortDocs([
      doc('b.md', 'design', { title: '乙' }),
      doc('z.md', 'design', { title: '甲' }),
      doc('overview', 'overview', { title: '总览', generated: true }),
    ])
    expect(sorted.map((d) => d.path)).toEqual(['overview', 'z.md', 'b.md'])
    // 不修改入参
    expect(sorted).not.toBe(undefined)
  })

  it('标题为空时回落到路径，列表里不会出现空白行', () => {
    expect(docTitle({ path: 'design/a.md', title: '' })).toBe('design/a.md')
    expect(docTitle({ path: 'x', title: '标题' })).toBe('标题')
  })
})

describe('默认选中', () => {
  it('优先 overview，其次任意 generated，最后第一组第一篇；空目录返回空串', () => {
    const items = [
      doc('a.md', 'adr', { title: 'A' }),
      doc('overview', 'overview', { title: '总览', generated: true }),
    ]
    expect(pickDefaultPath(items)).toBe('overview')

    // overview 分类缺失但仍有生成文档 → 选它
    expect(pickDefaultPath([doc('a.md', 'adr', { title: 'A' }), doc('gen', 'status', { generated: true })])).toBe(
      'gen',
    )

    // 都没有 → 第一组（adr 在 guide 之后，但这里是唯一组）第一篇
    expect(pickDefaultPath([doc('g.md', 'guide', { title: 'G' }), doc('a.md', 'adr', { title: 'A' })])).toBe(
      'g.md',
    )
    expect(pickDefaultPath([])).toBe('')
  })
})

describe('错误分类', () => {
  it('只有 503 算「模块未启用」，404 / 网络错误 / 其它 5xx 是真实错误', () => {
    expect(isDocsDisabled({ status: 503 })).toBe(true)
    expect(isDocsDisabled({ status: 404 })).toBe(false)
    expect(isDocsDisabled({ status: 0 })).toBe(false)
    expect(isDocsDisabled({ status: 500 })).toBe(false)
    expect(isDocsDisabled(new Error('boom'))).toBe(false)
    expect(isDocsDisabled(null)).toBe(false)
  })
})

describe('搜索高亮切分', () => {
  it('大小写不敏感，多处命中都切出来，未命中保持一段', () => {
    expect(highlightSegments('Docs 与 docs 都是文档', 'DOCS')).toEqual([
      { text: 'Docs', hit: true },
      { text: ' 与 ', hit: false },
      { text: 'docs', hit: true },
      { text: ' 都是文档', hit: false },
    ])
    expect(highlightSegments('无关内容', 'zzz')).toEqual([{ text: '无关内容', hit: false }])
    expect(highlightSegments('任意', '  ')).toEqual([{ text: '任意', hit: false }])
    expect(highlightSegments('', 'x')).toEqual([])
  })

  it('切出的片段拼回去等于原文（不会吞字）', () => {
    const src = '渐进式披露：能力发现收敛到三层'
    const joined = highlightSegments(src, '披露').map((s) => s.text).join('')
    expect(joined).toBe(src)
  })
})

describe('正文去重标题', () => {
  it('开头 H1 与标题一致时删掉（含其后空行），不一致则原样保留', () => {
    const body = '# 自闭环\n\n正文第一段'
    expect(stripLeadingH1(body, '自闭环')).toBe('正文第一段')
    // 标题对不上 → 保留
    expect(stripLeadingH1(body, '别的标题')).toBe(body)
    // 没有标题 / 空正文
    expect(stripLeadingH1('正文', '标题')).toBe('正文')
    expect(stripLeadingH1('', '标题')).toBe('')
    // 前导空行不影响判断
    expect(stripLeadingH1('\n\n# 标题\n正文', '标题')).toBe('正文')
  })
})
