import type { RunEvent } from './types'

// 事件 → 视觉块。
//
// ACP 的 session/update 是「增量流」：文本按 token 来、工具调用分 start/update 两段、
// 审批分 req/result 两跳。直接按事件列表渲染会出现几百个碎片节点，也没法把
// 「一段回答 + 它中间调用的工具」组织成一个连贯的块。所以先折叠一次：
// 相邻增量合并、工具与审批按 toolCallId 归并。

export type ToolBlock = {
  type: 'tool'
  id: string
  title: string
  kind: string
  status: string
  input?: unknown
  output?: unknown
  /** ACP 的 content[]：文本片段、diff、terminal 引用等。 */
  content?: unknown
}

export type ApprovalBlock = {
  type: 'approval'
  toolCallId: string
  title: string
  kind: string
  policy: string
  pending: boolean
  decision?: string
  by?: string
}

export type Block =
  | { type: 'message'; text: string }
  | { type: 'thought'; text: string }
  | ToolBlock
  | ApprovalBlock
  | { type: 'error'; text: string }
  | { type: 'log'; kind: string }

export function buildBlocks(events: RunEvent[]): Block[] {
  const out: Block[] = []
  const tools = new Map<string, ToolBlock>()
  const approvals = new Map<string, ApprovalBlock>()

  for (const ev of events) {
    const p = ev.payload || {}
    switch (ev.type) {
      case 'message_delta': {
        const t = String(p.text ?? '')
        if (!t) break
        const last = out[out.length - 1]
        if (last && last.type === 'message') last.text += t
        else out.push({ type: 'message', text: t })
        break
      }
      case 'thought_delta': {
        const t = String(p.text ?? '')
        if (!t) break
        const last = out[out.length - 1]
        if (last && last.type === 'thought') last.text += t
        else out.push({ type: 'thought', text: t })
        break
      }
      case 'tool_call_start': {
        const id = String(p.toolCallId ?? '')
        // 同一个 toolCallId 的 start 来两次（重放 / 续传 / 上游重发）时不能再推一个块。
        // 推了的话后续 update 只作用于最后一块，前一块永久停在 pending——
        // 界面上就是「一次调用显示成两条，其中一条永远转圈」。
        const existed = tools.get(id)
        if (existed) {
          if (p.title) existed.title = String(p.title)
          if (p.kind) existed.kind = String(p.kind)
          if (p.status) existed.status = String(p.status)
          if (p.rawInput !== undefined && p.rawInput !== null) existed.input = p.rawInput
          if (p.rawOutput !== undefined && p.rawOutput !== null) existed.output = p.rawOutput
          if (p.content !== undefined && p.content !== null) existed.content = p.content
          break
        }
        const b: ToolBlock = {
          type: 'tool',
          id,
          title: String(p.title ?? '工具调用'),
          kind: String(p.kind ?? 'other'),
          status: String(p.status ?? 'pending'),
          input: p.rawInput,
          output: p.rawOutput,
          content: p.content ?? undefined,
        }
        tools.set(b.id, b)
        out.push(b)
        break
      }
      case 'tool_call_update': {
        const id = String(p.toolCallId ?? '')
        let b = tools.get(id)
        if (!b) {
          // 流是半路接上的（页面刷新/续传），只有 update 没有 start。
          // 补一个占位块，而不是把这条更新丢掉——丢掉的话界面上会凭空少一次调用。
          b = { type: 'tool', id, title: String(p.title ?? '工具调用'), kind: 'other', status: 'pending' }
          tools.set(id, b)
          out.push(b)
        }
        if (p.title) b.title = String(p.title)
        // 有的 agent 在 update 里才给 kind；不要用空值覆盖已知的。
        if (p.kind) b.kind = String(p.kind)
        if (p.status) b.status = String(p.status)
        if (p.rawOutput !== undefined && p.rawOutput !== null) b.output = p.rawOutput
        if (p.content !== undefined && p.content !== null) b.content = p.content
        break
      }
      case 'approval_req': {
        const id = String(p.toolCallId ?? '')
        // 标题兜底链：上游给的 title → 同一个 toolCallId 的工具调用标题 → 调用 id → 固定文案。
        //
        // 为什么在折叠层就把兜底做完：dsh 的权限桥只发 toolCallId 不带 title，
        // 之前靠渲染层回查，结果是「任何不走那个组件的消费方（顶部通知条、导出、
        // 审计视图）都会拿到空标题」。把不变量放在数据层，渲染层就不可能忘。
        const fromTool = tools.get(id)
        const a: ApprovalBlock = {
          type: 'approval',
          toolCallId: id,
          title: String(p.title ?? '') || fromTool?.title || id || '权限请求',
          kind: String(p.kind ?? '') || fromTool?.kind || 'other',
          policy: String(p.policy ?? ''),
          // 缺 decision 一律当作「还没决定」。默认成已处理会让界面直接不显示
          // 允许/拒绝按钮 —— 审批能力静默消失，这是最不能出的错。
          pending: (p.decision ?? 'pending') === 'pending',
          decision: p.decision,
        }
        approvals.set(a.toolCallId, a)
        out.push(a)
        break
      }
      case 'approval_result': {
        const id = String(p.toolCallId ?? '')
        const a = approvals.get(id)
        if (a) {
          a.pending = false
          a.decision = String(p.decision ?? '')
          a.by = String(p.by ?? '')
        }
        break
      }
      case 'error': {
        out.push({ type: 'error', text: String(p.message ?? '未知错误') })
        break
      }
      case 'agent_log': {
        const kind = String(p.kind ?? p.error ?? 'log')
        out.push({ type: 'log', kind })
        break
      }
      default:
        break
    }
  }
  return out
}

/** run 的最新状态：以最后一条 run_state 为准，没有就用 run 元数据。 */
export function lastStatus(events: RunEvent[], fallback: string): string {
  for (let i = events.length - 1; i >= 0; i--) {
    const ev = events[i]
    if (ev.type === 'run_state' && ev.payload?.status) return String(ev.payload.status)
  }
  return fallback
}

/** 展示工具入参：有 command 就只显示命令，否则 JSON。 */
export function formatValue(v: unknown): string {
  if (v === undefined || v === null) return ''
  if (typeof v === 'string') return v
  if (typeof v === 'object') {
    const o = v as Record<string, unknown>
    if (typeof o.command === 'string') {
      const args = Array.isArray(o.args) ? (o.args as unknown[]).join(' ') : ''
      return args ? `${o.command} ${args}` : o.command
    }
    try {
      return JSON.stringify(v, null, 2)
    } catch {
      return String(v)
    }
  }
  return String(v)
}

/**
 * 把工具产出拍平成一段可读文本。
 *
 * 为什么要这一层：ACP 的 content[] 是结构化数组——
 *   {type:'content', content:{type:'text', text:'hi\n'}}   ← 普通输出
 *   {type:'diff', path:'a.py', oldText, newText}          ← 文件改动
 *   {type:'terminal', terminalId}                         ← 终端引用（我们不内嵌终端）
 * 而 rawOutput 在不同 agent 上有时是字符串、有时是对象、有时干脆没有。
 * 界面上只需要「这次调用到底产出了什么」，所以在这里统一成文本；
 * 解析不出来的部分退回 JSON，宁难看也别丢信息。
 */
export function toolOutput(b: ToolBlock): string {
  const parts: string[] = []
  const push = (s: string) => {
    if (s.trim()) parts.push(s.replace(/\s+$/, ''))
  }

  const items = b.content
  if (Array.isArray(items)) {
    for (const it of items) {
      if (!it || typeof it !== 'object') continue
      const o = it as Record<string, unknown>
      if (o.type === 'content') {
        const c = o.content as Record<string, unknown> | undefined
        if (c && typeof c.text === 'string') push(c.text)
      } else if (o.type === 'diff') {
        const path = typeof o.path === 'string' ? o.path : '(文件)'
        const lines = typeof o.newText === 'string' ? o.newText.split('\n').length : 0
        push(`编辑 ${path}${lines ? `（${lines} 行）` : ''}`)
      } else if (o.type === 'terminal') {
        push('（终端输出见上方命令）')
      } else if (typeof o.text === 'string') {
        push(o.text)
      }
    }
  }

  if (!parts.length) {
    const out = formatValue(b.output)
    if (out) parts.push(out)
  }
  return parts.join('\n\n')
}
