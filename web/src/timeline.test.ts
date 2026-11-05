import { describe, expect, it } from 'vitest'
import { buildBlocks, formatValue, lastStatus, toolOutput } from './timeline'
import type { ApprovalBlock, Block, ToolBlock } from './timeline'
import type { RunEvent } from './types'

// 事件构造器。seq 递增只是为了贴近真实数据；buildBlocks 不读 seq。
let seq = 0
function ev(type: string, payload: any = {}): RunEvent {
  seq += 1
  return { seq, type, payload, createdAt: '2026-01-01T00:00:00Z' }
}

function tools(blocks: Block[]): ToolBlock[] {
  return blocks.filter((b): b is ToolBlock => b.type === 'tool')
}

function approvalOf(blocks: Block[]): ApprovalBlock {
  const a = blocks.find((b): b is ApprovalBlock => b.type === 'approval')
  if (!a) throw new Error('测试数据里没有 approval 块')
  return a
}

describe('buildBlocks：一次完整对话的折叠', () => {
  // 防的回归：任何一次「多插一块 / 少插一块 / 块类型串了」都会让对话流错位，
  // 而这类错位在界面上表现为「工具卡片跑到回答前面」之类的怪象，很难靠肉眼定位。
  it('按出现顺序折叠出 消息/思考/工具/审批/日志/错误 七种块', () => {
    const blocks = buildBlocks([
      ev('message_delta', { text: '我来' }),
      ev('message_delta', { text: '看看目录。' }),
      ev('thought_delta', { text: '先列' }),
      ev('thought_delta', { text: '一下' }),
      ev('tool_call_start', {
        toolCallId: 'tc1',
        title: 'ls -la',
        kind: 'execute',
        status: 'pending',
        rawInput: { command: 'ls', args: ['-la'] },
      }),
      ev('approval_req', { toolCallId: 'tc1', kind: 'execute', policy: 'interactive', decision: 'pending' }),
      ev('approval_result', { toolCallId: 'tc1', decision: 'allow', by: 'u-1' }),
      ev('tool_call_update', {
        toolCallId: 'tc1',
        status: 'completed',
        content: [{ type: 'content', content: { type: 'text', text: 'a.txt\nb.txt' } }],
      }),
      ev('message_delta', { text: '目录里' }),
      ev('message_delta', { text: '有两个文件。' }),
      ev('agent_log', { kind: 'plan' }),
      ev('error', { message: '模型限流' }),
    ])

    expect(blocks.map((b) => b.type)).toEqual([
      'message',
      'thought',
      'tool',
      'approval',
      'message',
      'log',
      'error',
    ])

    // 相邻增量要合并，而不是一个 token 一个块。
    expect(blocks[0]).toEqual({ type: 'message', text: '我来看看目录。' })
    expect(blocks[1]).toEqual({ type: 'thought', text: '先列一下' })
    // 工具块与审批块按 toolCallId 关联，各自只有一块。
    expect(tools(blocks)).toHaveLength(1)
    expect(tools(blocks)[0].id).toBe('tc1')
    expect(tools(blocks)[0].status).toBe('completed')
    // 工具之后的文本要开新块，不能并回工具之前那条消息。
    expect(blocks[4]).toEqual({ type: 'message', text: '目录里有两个文件。' })
    expect(blocks[5]).toEqual({ type: 'log', kind: 'plan' })
    expect(blocks[6]).toEqual({ type: 'error', text: '模型限流' })
  })

  it('空事件数组返回空数组（调用方直接渲染，不能是 null/undefined）', () => {
    const blocks = buildBlocks([])
    expect(Array.isArray(blocks)).toBe(true)
    expect(blocks).toHaveLength(0)
  })
})

describe('buildBlocks：工具卡片不空白（一期回归 1）', () => {
  // 一期 bug：工具卡片只剩一个空壳，标题和内容都没有。
  // 成因是 start 只带 title、update 只带 content，两者到达顺序不固定。
  it('先 tool_call_start（只有 title）后 tool_call_update（补 content）→ 同一个块，标题与内容都在', () => {
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc1', title: '读取 config.yaml', kind: 'read', status: 'in_progress' }),
      ev('tool_call_update', {
        toolCallId: 'tc1',
        status: 'completed',
        content: [{ type: 'content', content: { type: 'text', text: 'port: 7788\n' } }],
      }),
    ])

    expect(tools(blocks)).toHaveLength(1)
    const b = tools(blocks)[0]
    expect(b.title).toBe('读取 config.yaml')
    expect(b.kind).toBe('read')
    expect(b.status).toBe('completed')
    expect(b.content).toBeDefined()
    // 卡片正文走 toolOutput；title 与正文任一为空都会退化成「空白卡片」。
    expect(b.title).not.toBe('')
    expect(toolOutput(b)).toBe('port: 7788')
  })

  it('只有 tool_call_update（刷新/续传半路接上）→ 也要补出占位块并带上内容', () => {
    const blocks = buildBlocks([
      ev('tool_call_update', {
        toolCallId: 'tc2',
        title: 'grep -rn TODO',
        kind: 'search',
        status: 'completed',
        content: [{ type: 'content', content: { type: 'text', text: 'a.ts:12' } }],
      }),
    ])

    expect(tools(blocks)).toHaveLength(1)
    const b = tools(blocks)[0]
    expect(b.id).toBe('tc2')
    expect(b.title).toBe('grep -rn TODO')
    expect(b.status).toBe('completed')
    expect(toolOutput(b)).toBe('a.ts:12')
  })

  it('start 与 update 都不给 title 时退到「工具调用」而不是空字符串', () => {
    // 缺标题时如果留空，卡片头部就是一片空白（正是回归时的观感）。
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc3' }),
      ev('tool_call_update', { toolCallId: 'tc3', status: 'completed' }),
    ])
    expect(tools(blocks)[0].title).toBe('工具调用')
  })

  it('update 里的空 title/kind 不覆盖 start 已知的值', () => {
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc4', title: '写入 a.txt', kind: 'edit' }),
      ev('tool_call_update', { toolCallId: 'tc4', title: '', kind: '', status: 'completed' }),
    ])
    expect(tools(blocks)[0].title).toBe('写入 a.txt')
    expect(tools(blocks)[0].kind).toBe('edit')
    expect(tools(blocks)[0].status).toBe('completed')
  })

  it('update 的 rawOutput 只在非空时覆盖，undefined 不清掉已有产出', () => {
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc5', title: 'echo hi', rawOutput: 'hi' }),
      ev('tool_call_update', { toolCallId: 'tc5', status: 'completed' }),
    ])
    expect(toolOutput(tools(blocks)[0])).toBe('hi')
  })

  it('同一 id 重复 start 只产出一个块，后续字段合并进去', () => {
    // 事件流按 seq 去重（App.tsx），正常路径不会重复；但重放/续传/上游重发
    // 都可能让同一个 toolCallId 出现两次 start。折叠层必须自己合流，否则
    // 后续 update 只作用于最后一块，前一块永久停在 pending——
    // 界面上就是「一次调用显示成两条，其中一条永远转圈」。
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc6', title: '第一次', kind: 'edit' }),
      ev('tool_call_start', { toolCallId: 'tc6', title: '第二次', rawOutput: 'done' }),
    ])
    expect(tools(blocks)).toHaveLength(1)
    expect(tools(blocks)[0].title).toBe('第二次')
    expect(tools(blocks)[0].kind).toBe('edit') // 第二次没给 kind，不能把已知值冲成 other
    expect(tools(blocks)[0].output).toBe('done')
  })
})

describe('buildBlocks：审批卡片（一期回归 2）', () => {
  // 一期 bug：审批卡片只有「需要你确认」，没有「要执行什么」。
  // dsh 的权限请求只带 toolCallId，标题得靠同 id 的 tool 块回查。
  // 回查发生在渲染层（Turn.tsx 的 byId + `b.title || tool?.title || b.toolCallId`），
  // 这里守住它依赖的两个前提：审批块保留 toolCallId、同 id 的工具块带着标题。
  it('审批块保留 toolCallId，且能按该 id 回查到工具标题', () => {
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc9', title: '写入 /tmp/x', kind: 'edit' }),
      ev('approval_req', { toolCallId: 'tc9', policy: 'interactive', decision: 'pending' }),
    ])

    const a = approvalOf(blocks)
    expect(a.toolCallId).toBe('tc9')
    expect(a.pending).toBe(true)

    const byId = new Map(tools(blocks).map((t) => [t.id, t]))
    expect(byId.get(a.toolCallId)?.title).toBe('写入 /tmp/x')
    expect(byId.get(a.toolCallId)?.kind).toBe('edit')
  })

  it('审批结果回到同一个块上：pending 落回 false，decision/by 被写入', () => {
    // 防的回归：approval_result 另起一块 → 界面上出现「一条待确认 + 一条已处理」两张卡。
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tc9', title: 'rm -rf build' }),
      ev('approval_req', { toolCallId: 'tc9', decision: 'pending' }),
      ev('approval_result', { toolCallId: 'tc9', decision: 'deny', by: 'u-1' }),
    ])

    expect(blocks.filter((b) => b.type === 'approval')).toHaveLength(1)
    const a = approvalOf(blocks)
    expect(a.pending).toBe(false)
    expect(a.decision).toBe('deny')
    expect(a.by).toBe('u-1')
    // 处理完之后仍然能回查标题（已处理的审批收成一行时要用）。
    expect(tools(blocks)[0].title).toBe('rm -rf build')
  })

  it('只有 toolCallId 时由折叠层回查工具标题，不留给渲染层兜底', () => {
    // dsh 的权限桥只发 toolCallId。回查必须在数据层完成，否则任何不走
    // Turn.tsx 的消费方（顶部通知条、导出、审计视图）拿到的都是空标题。
    const blocks = buildBlocks([
      ev('tool_call_start', { toolCallId: 'tcA', title: '写入 a.txt', kind: 'edit' }),
      ev('approval_req', { toolCallId: 'tcA' }),
    ])
    expect(approvalOf(blocks).title).toBe('写入 a.txt')
    expect(approvalOf(blocks).kind).toBe('edit')
  })

  it('回查不到工具块时也给出非空标题（退到调用 id）', () => {
    const blocks = buildBlocks([ev('approval_req', { toolCallId: 'tcZ' })])
    expect(approvalOf(blocks).title).toBe('tcZ')
  })

  it('approval_req 缺 decision 时按 pending 处理（宁可多问一次）', () => {
    // 缺字段时若默认成「已处理」，界面会直接不显示允许/拒绝按钮——
    // 审批能力静默消失。这里必须偏向安全侧。
    const blocks = buildBlocks([ev('approval_req', { toolCallId: 'tcB' })])
    expect(approvalOf(blocks).pending).toBe(true)
  })

  it('没有对应 approval_req 的 approval_result 不产生块（不乱插卡）', () => {
    const blocks = buildBlocks([ev('approval_result', { toolCallId: 'ghost', decision: 'allow' })])
    expect(blocks).toHaveLength(0)
  })
})

describe('buildBlocks：高频增量合并', () => {
  it('连续 20 个文本增量折叠成一个消息块，文本顺序拼完整', () => {
    // 防的回归：每个 delta 一块 → 几百个碎片节点，滚动条被撑爆。
    const events = Array.from({ length: 20 }, (_, i) => ev('message_delta', { text: String(i) }))
    const blocks = buildBlocks(events)
    expect(blocks).toHaveLength(1)
    expect(blocks[0]).toEqual({ type: 'message', text: Array.from({ length: 20 }, (_, i) => String(i)).join('') })
  })

  it('思考与正文交替时不互相吞并', () => {
    const blocks = buildBlocks([
      ev('thought_delta', { text: '想 1' }),
      ev('message_delta', { text: '说 1' }),
      ev('thought_delta', { text: '想 2' }),
    ])
    expect(blocks).toEqual([
      { type: 'thought', text: '想 1' },
      { type: 'message', text: '说 1' },
      { type: 'thought', text: '想 2' },
    ])
  })

  it('空文本增量被丢弃，不会产生空块', () => {
    const blocks = buildBlocks([
      ev('message_delta', { text: '' }),
      ev('thought_delta', {}),
      ev('message_delta', { text: '有内容' }),
    ])
    expect(blocks).toEqual([{ type: 'message', text: '有内容' }])
  })
})

describe('buildBlocks：前向兼容', () => {
  it('未知事件类型被忽略，且不破坏已有块', () => {
    // 上游新增一种 session/update（如 plan / current_mode_update）时，
    // 老前端只应少显示一点东西，不能崩、也不能把已有块冲掉。
    const blocks = buildBlocks([
      ev('message_delta', { text: '前' }),
      ev('plan_update', { steps: [] }),
      ev('run_state', { status: 'running' }),
      ev('some_future_event', { anything: true }),
      ev('message_delta', { text: '后' }),
    ])
    expect(blocks).toEqual([{ type: 'message', text: '前后' }])
  })

  it('payload 为 null/缺省时不抛异常', () => {
    const events: RunEvent[] = [
      { seq: 1, type: 'message_delta', payload: null, createdAt: '' },
      { seq: 2, type: 'tool_call_update', payload: null, createdAt: '' },
      { seq: 3, type: 'agent_log', payload: null, createdAt: '' },
    ]
    expect(() => buildBlocks(events)).not.toThrow()
    expect(buildBlocks(events).map((b) => b.type)).toEqual(['tool', 'log'])
  })

  it('error 缺 message 时给「未知错误」而不是空串', () => {
    expect(buildBlocks([ev('error', {})])).toEqual([{ type: 'error', text: '未知错误' }])
  })

  it('agent_log 没有 kind 时依次退到 error 字段与 "log"', () => {
    expect(buildBlocks([ev('agent_log', { error: '无法解析 update' })])).toEqual([
      { type: 'log', kind: '无法解析 update' },
    ])
    expect(buildBlocks([ev('agent_log', {})])).toEqual([{ type: 'log', kind: 'log' }])
  })
})

describe('lastStatus', () => {
  it('取最后一条 run_state 的状态', () => {
    const events = [
      ev('run_state', { status: 'running' }),
      ev('message_delta', { text: 'x' }),
      ev('run_state', { status: 'waiting_approval' }),
    ]
    expect(lastStatus(events, 'queued')).toBe('waiting_approval')
  })

  it('没有 run_state 时退回 run 元数据里的状态', () => {
    expect(lastStatus([ev('message_delta', { text: 'x' })], 'running')).toBe('running')
    expect(lastStatus([], 'queued')).toBe('queued')
  })

  it('run_state 缺 status 时继续往前找', () => {
    const events = [ev('run_state', { status: 'completed' }), ev('run_state', {})]
    expect(lastStatus(events, 'queued')).toBe('completed')
  })
})

describe('formatValue：工具入参展示', () => {
  it('有 command 时只显示命令（含 args）', () => {
    expect(formatValue({ command: 'ls', args: ['-la', '/tmp'] })).toBe('ls -la /tmp')
    expect(formatValue({ command: 'pwd' })).toBe('pwd')
  })

  it('其他对象走缩进 JSON，字符串原样返回', () => {
    expect(formatValue({ path: 'a.txt' })).toBe('{\n  "path": "a.txt"\n}')
    expect(formatValue('已经 是 字符串')).toBe('已经 是 字符串')
  })

  it('null/undefined 返回空串（调用方据此决定要不要渲染「输入」区）', () => {
    expect(formatValue(undefined)).toBe('')
    expect(formatValue(null)).toBe('')
  })
})

describe('toolOutput：把 content[] 拍平成文本', () => {
  const base = (over: Partial<ToolBlock>): ToolBlock => ({
    type: 'tool',
    id: 'tc',
    title: 't',
    kind: 'other',
    status: 'completed',
    ...over,
  })

  it('content 里的文本片段按顺序用空行拼接', () => {
    const b = base({
      content: [
        { type: 'content', content: { type: 'text', text: '第一段\n' } },
        { type: 'content', content: { type: 'text', text: '第二段' } },
      ],
    })
    expect(toolOutput(b)).toBe('第一段\n\n第二段')
  })

  it('diff 展示路径与行数，terminal 给出提示而非空白', () => {
    const b = base({
      content: [
        { type: 'diff', path: 'a.py', newText: 'x\ny\nz' },
        { type: 'terminal', terminalId: 'term-1' },
      ],
    })
    expect(toolOutput(b)).toBe('编辑 a.py（3 行）\n\n（终端输出见上方命令）')
  })

  it('content 为空时退回 rawOutput', () => {
    expect(toolOutput(base({ output: 'hello' }))).toBe('hello')
    expect(toolOutput(base({ output: { ok: true } }))).toBe('{\n  "ok": true\n}')
  })

  it('content 与 output 都没有时返回空串，不抛异常', () => {
    expect(toolOutput(base({}))).toBe('')
    expect(toolOutput(base({ content: [] }))).toBe('')
  })
})
