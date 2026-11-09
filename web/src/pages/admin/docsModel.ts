// 文档页（/admin/docs）的纯展示逻辑。
//
// 拆成 .ts（不含 JSX）的理由和 loopModel.ts 一样：vitest 跑在 node 环境、
// 不引 React 插件，分组 / 排序 / 默认选中 / 高亮 / 503 判定才能直接单测。
// 页面本身只负责取数与渲染。

import type { DocMeta } from '../../api'

// ── 分类 ─────────────────────────────────────────────────────

/** 目录里的分组顺序。overview 固定在最前——「当前做了什么功能」是入口。 */
export const CATEGORY_ORDER = [
  'overview',
  'guide',
  'design',
  'adr',
  'ops',
  'status',
  'history',
] as const

/** 分类 → 中文分组标题。未知分类不吞掉，原样显示在后。 */
export const CATEGORY_LABEL: Record<string, string> = {
  overview: '功能总览',
  guide: '使用指南',
  design: '设计',
  adr: '决策记录',
  ops: '运维',
  status: '状态评估',
  history: '历史',
}

export function categoryLabel(category: string): string {
  return CATEGORY_LABEL[category] || category || '其它'
}

/** 文档标题：空标题回落到路径（文件名），列表里不会出现空白行。 */
export function docTitle(doc: Pick<DocMeta, 'title' | 'path'>): string {
  return doc.title || doc.path || '(未命名)'
}

// ── 排序 / 分组 ───────────────────────────────────────────────

/**
 * 组内排序：运行时生成的总览永远第一，其余按标题排序（中文按拼音）。
 *
 * 后端已经排过一次，这里再排是为了让前端逻辑可测、也不依赖接口的排序细节。
 */
export function sortDocs(items: DocMeta[]): DocMeta[] {
  return items.slice().sort((a, b) => {
    if (Boolean(a.generated) !== Boolean(b.generated)) return a.generated ? -1 : 1
    return docTitle(a).localeCompare(docTitle(b), 'zh-Hans-CN')
  })
}

export type DocGroup = { category: string; label: string; items: DocMeta[] }

/** 按分类分组。已知分类按 CATEGORY_ORDER，未知分类排在最后（按名字排）。 */
export function groupDocs(items: DocMeta[]): DocGroup[] {
  const buckets = new Map<string, DocMeta[]>()
  for (const it of items || []) {
    const key = it.category || ''
    const list = buckets.get(key)
    if (list) list.push(it)
    else buckets.set(key, [it])
  }

  const known = CATEGORY_ORDER.filter((c) => buckets.has(c)).map((c) => ({
    category: c,
    label: categoryLabel(c),
    items: sortDocs(buckets.get(c) as DocMeta[]),
  }))
  const extra = [...buckets.keys()]
    .filter((c) => !(CATEGORY_ORDER as readonly string[]).includes(c))
    .sort((a, b) => categoryLabel(a).localeCompare(categoryLabel(b), 'zh-Hans-CN'))
    .map((c) => ({
      category: c,
      label: categoryLabel(c),
      items: sortDocs(buckets.get(c) as DocMeta[]),
    }))

  return [...known, ...extra]
}

/**
 * 默认选中的文档：功能总览优先（它是「当前做了什么」的入口），
 * 其次任意一篇 generated，最后退到第一组第一篇。目录为空时返回 ''。
 */
export function pickDefaultPath(items: DocMeta[]): string {
  if (!items || items.length === 0) return ''
  const overview =
    items.find((d) => d.category === 'overview') || items.find((d) => d.generated)
  if (overview) return overview.path
  const first = groupDocs(items)[0]
  return first?.items[0]?.path || items[0].path || ''
}

// ── 错误分类 ─────────────────────────────────────────────────

/**
 * 「文档模块未启用」判定。
 *
 * 只有 503 是未启用：它是后端在 h.cfg.Docs == nil 时明确回的状态。
 * 404（文档不存在）、0（网络不通）、其它 5xx 都是真实错误，必须把
 * 原始信息显示出来——一律当「未启用」会让一次手误的路径变成
 * 「去改部署」的错误结论。
 */
export function isDocsDisabled(e: unknown): boolean {
  return (e as { status?: number } | null | undefined)?.status === 503
}

// ── 搜索高亮 ─────────────────────────────────────────────────

export type Segment = { text: string; hit: boolean }

/**
 * 把命中片段切成「普通 / 命中」两段，交给 React 渲染 <mark>。
 *
 * 刻意不返回 HTML 字符串：高亮内容来自接口，拼 HTML 会多一条注入面；
 * 切分后由 React 按文本节点渲染，天然安全。
 */
export function highlightSegments(text: string, query: string): Segment[] {
  const src = String(text ?? '')
  const q = String(query ?? '').trim()
  if (!src) return []
  if (!q) return [{ text: src, hit: false }]

  const hay = src.toLowerCase()
  const needle = q.toLowerCase()
  if (!needle) return [{ text: src, hit: false }]

  const out: Segment[] = []
  let at = 0
  for (;;) {
    const idx = hay.indexOf(needle, at)
    if (idx < 0) break
    if (idx > at) out.push({ text: src.slice(at, idx), hit: false })
    out.push({ text: src.slice(idx, idx + needle.length), hit: true })
    at = idx + needle.length
  }
  if (out.length === 0) return [{ text: src, hit: false }]
  if (at < src.length) out.push({ text: src.slice(at), hit: false })
  return out
}

// ── 正文预处理 ───────────────────────────────────────────────

/**
 * 去掉正文开头与标题重复的一级标题。
 *
 * 页面顶部已经显示标题 / 时间 / 路径，正文再出现一次同名 H1 只是重复。
 * 只在**确实与标题一致**时删（比较时压掉首尾与连续空白）；标题对不上
 * 就原样保留——那说明 H1 是正文的一部分，不是页码。
 */
export function stripLeadingH1(body: string, title: string): string {
  const text = String(body ?? '').replace(/\r\n?/g, '\n')
  if (!text) return ''
  const lines = text.split('\n')

  let k = 0
  while (k < lines.length && !lines[k].trim()) k++
  if (k >= lines.length) return text

  const m = /^#\s+(.+?)\s*#*\s*$/.exec(lines[k])
  if (!m) return text

  const norm = (s: string) => s.replace(/\s+/g, ' ').trim()
  if (norm(m[1]) !== norm(title || '')) return text

  lines.splice(k, 1)
  // 标题前后的空行一起删掉：正文顶部不该再留一块空白。
  while (k < lines.length && !lines[k].trim()) lines.splice(k, 1)
  let start = 0
  while (start < lines.length && !lines[start].trim()) start++
  return lines.slice(start).join('\n')
}
