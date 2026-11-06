// 行动账本页面的纯展示逻辑。
//
// 单独拆成 .ts（不含 JSX）：vitest 跑在 node 环境、不引 React 插件，
// 这些映射/计数/空态判断才能在单测里直接 import。页面本身负责取数与渲染。

import type { ActivityEntry } from '../../api'

/** 一个 kind 的展示元数据。glyph 用单个汉字当圆点里的「图标」，避免 emoji。 */
export type KindMeta = { label: string; glyph: string; tone: string }

const KIND_META: Record<string, KindMeta> = {
  plan: { label: '任务', glyph: '任', tone: 'plan' },
  tool: { label: '工具', glyph: '工', tone: 'tool' },
  approval: { label: '审批', glyph: '审', tone: 'approval' },
  egress: { label: '出网', glyph: '网', tone: 'egress' },
  message: { label: '回复', glyph: '答', tone: 'message' },
  child: { label: '子任务', glyph: '子', tone: 'child' },
  state: { label: '状态', glyph: '态', tone: 'state' },
  error: { label: '错误', glyph: '错', tone: 'error' },
}

const UNKNOWN_KIND: KindMeta = { label: '事件', glyph: '•', tone: 'state' }

/** kind → 图标/标签/色调；未知 kind 走中性样式，不标红冒充错误。 */
export function kindMeta(kind: string): KindMeta {
  return KIND_META[kind] ?? { ...UNKNOWN_KIND, label: kind || UNKNOWN_KIND.label }
}

export type StatusMeta = { label: string; tone: string }

const STATUS_META: Record<string, StatusMeta> = {
  done: { label: '已完成', tone: 'done' },
  running: { label: '进行中', tone: 'running' },
  blocked: { label: '阻塞', tone: 'blocked' },
  failed: { label: '失败', tone: 'failed' },
}

/** status → 徽标文案/色调；未知值退到「进行中」的中性样式。 */
export function statusMeta(status: string): StatusMeta {
  return STATUS_META[status] ?? { label: status || '—', tone: 'running' }
}

export type StatusCounts = {
  done: number
  running: number
  blocked: number
  failed: number
  /** 最新一条 blocked 的标题；没有阻塞项时为空串。 */
  current: string
}

/**
 * 从条目里算四个计数 + 「现在卡在哪」。
 * run 视图后端直接给 summary，但 session 视图只回 entries，这里在前端现算。
 * current 取「时间最靠后」的 blocked，而不是简单取最后一个——entries 理论上
 * 已按时间升序，显式比一次更稳。
 */
export function countStatuses(entries: ActivityEntry[]): StatusCounts {
  const c: StatusCounts = { done: 0, running: 0, blocked: 0, failed: 0, current: '' }
  let latestBlockedTime = ''
  for (const e of entries || []) {
    switch (e.status) {
      case 'done':
        c.done++
        break
      case 'running':
        c.running++
        break
      case 'failed':
        c.failed++
        break
      case 'blocked':
        c.blocked++
        if (!c.current || (e.time && e.time >= latestBlockedTime)) {
          c.current = e.title
          latestBlockedTime = e.time || ''
        }
        break
      default:
        break
    }
  }
  return c
}

export type EmptyState = 'no-id' | 'disabled' | 'error' | 'no-entries'

/**
 * 空态文案。两处措辞是产品约定，别随便改：
 * - 没输入 id 时明确告诉用户「输入 run id 查看」，而不是画一张空图表；
 * - 未启用（503）时明确说「能力未启用」，空图表会被误读成「在跑、还没数据」。
 */
export function emptyHint(state: EmptyState, mode: 'run' | 'session'): string {
  switch (state) {
    case 'no-id':
      return mode === 'run' ? '输入 run id 查看该轮行动时间线' : '输入 session id 查看该会话行动时间线'
    case 'disabled':
      return '能力未启用'
    case 'no-entries':
      return '这个 id 下还没有行动记录'
    case 'error':
      return '加载失败，请重试'
  }
}
