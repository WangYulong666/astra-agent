import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { ApprovalBlock, Block, ToolBlock } from '../timeline'
import { formatValue, toolOutput } from '../timeline'
import { Markdown } from './Markdown'
import type { Run } from '../types'
import { STATUS_LABEL, isTerminal } from '../types'
import { IconCopy, IconReuse } from './ui'

const KIND_LABEL: Record<string, string> = {
  execute: 'exec',
  edit: 'edit',
  read: 'read',
  delete: 'del',
  move: 'move',
  search: 'find',
  fetch: 'net',
  think: 'think',
  other: 'tool',
}

const TOOL_STATUS: Record<string, { text: string; cls: string }> = {
  pending: { text: '待执行', cls: '' },
  in_progress: { text: '执行中', cls: '' },
  completed: { text: '完成', cls: 'done' },
  failed: { text: '失败', cls: 'fail' },
}

// ── 消息操作条 ─────────────────────────────────────────────
//
// 位置：桌面在气泡右上角外侧（绝对定位，hover 才出现，不吃布局）；
// 触屏在气泡下方展开（长按触发，讨论见 MsgRow 的注释）。
// 复制取的是 markdown 原文（blocks 里就是原文，不是渲染后的 DOM），
// 粘到别处是纯文本，不会带一堆 <span>。

function MsgRow({
  kind,
  body,
  actions,
}: {
  kind: 'user' | 'agent'
  body: ReactNode
  actions: ReactNode
}) {
  // 长按展开：手机上既没有 hover，也不该让每轮消息下方常驻一条按钮。
  // pointerdown 不行——滚动时会误触；用 touchstart + 定时器，手指一移动就取消。
  const [revealed, setRevealed] = useState(false)
  const timer = useRef<number | null>(null)
  const ref = useRef<HTMLDivElement>(null)

  const cancel = () => {
    if (timer.current !== null) {
      window.clearTimeout(timer.current)
      timer.current = null
    }
  }

  const start = () => {
    cancel()
    timer.current = window.setTimeout(() => setRevealed(true), 450)
  }

  useEffect(() => cancel, [])

  // 展开后点别处收回：触屏没有「移开鼠标」这个动作，必须给一条退路。
  useEffect(() => {
    if (!revealed) return
    const onDown = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) setRevealed(false)
    }
    document.addEventListener('pointerdown', onDown)
    return () => document.removeEventListener('pointerdown', onDown)
  }, [revealed])

  return (
    <div
      ref={ref}
      className={`msg-row ${kind} ${revealed ? 'revealed' : ''}`}
      onTouchStart={start}
      onTouchEnd={cancel}
      onTouchMove={cancel}
      onTouchCancel={cancel}
      onContextMenu={(e) => {
        // iOS/安卓长按会弹系统菜单，它挡住的是我们要的操作条。
        // 只在触屏上接管，桌面保留右键菜单（选中文字复制还要用）。
        if (window.matchMedia('(hover: none)').matches) {
          e.preventDefault()
          setRevealed(true)
        }
      }}
    >
      {body}
      {actions}
    </div>
  )
}

function MsgActions({
  text,
  onCopy,
  onReuse,
}: {
  text: string
  onCopy: (text: string) => void
  /** 用户消息才有：把这条重新放回输入框 */
  onReuse?: (text: string) => void
}) {
  return (
    <div className="msg-actions">
      <button
        className="mini-btn"
        title="复制纯文本"
        aria-label="复制这条消息"
        onClick={() => onCopy(text)}
      >
        <IconCopy />
      </button>
      {onReuse && (
        <button
          className="mini-btn"
          title="填回输入框（不会自动发送）"
          aria-label="把这条消息填回输入框"
          onClick={() => onReuse(text)}
        >
          <IconReuse />
        </button>
      )}
    </div>
  )
}


function ToolCard({ b }: { b: ToolBlock }) {
  // 失败的调用默认展开：用户最需要看的就是「它到底错在哪」。
  const [open, setOpen] = useState(b.status === 'failed')
  const input = formatValue(b.input)
  const output = toolOutput(b)
  const st = TOOL_STATUS[b.status] || { text: b.status, cls: '' }
  const hasBody = Boolean(input || output)

  return (
    <div className={`tool ${b.status === 'failed' ? 'failed' : ''}`}>
      <div className="tool-head" onClick={() => hasBody && setOpen(!open)}>
        <span className={`tool-kind ${b.kind}`}>{KIND_LABEL[b.kind] || b.kind}</span>
        <span className="tool-title" title={b.title}>
          {b.title}
        </span>
        <span className={`tool-status ${st.cls}`}>
          {b.status === 'in_progress' && <span className="spinner" />}
          {st.text}
        </span>
        {hasBody && <span className={`caret ${open ? 'open' : ''}`}>▶</span>}
      </div>
      {open && hasBody && (
        <div className="tool-body">
          {input && (
            <>
              <div className="tool-section-label">输入</div>
              <pre className="tool-pre">{input}</pre>
            </>
          )}
          {output && (
            <>
              <div className="tool-section-label">输出</div>
              <pre className="tool-pre">{output}</pre>
            </>
          )}
        </div>
      )}
    </div>
  )
}

function ApprovalCard({
  b,
  tool,
  onResolve,
}: {
  b: ApprovalBlock
  // dsh 的 ACP 权限桥只发 toolCallId（不带 title/kind），
  // 真正的意图在同 toolCallId 的 tool_call_start 上，所以从那里取。
  tool?: ToolBlock
  onResolve: (toolCallId: string, d: 'allow' | 'deny') => Promise<void>
}) {
  const [busy, setBusy] = useState<'allow' | 'deny' | null>(null)
  const decided = !b.pending && b.decision && b.decision !== 'pending'

  const kind = b.kind || tool?.kind || ''
  const title = b.title || tool?.title || b.toolCallId
  // 审批卡片最该让人看清的是「要执行什么」。agent 不给标题时就退到工具入参。
  const detail = formatValue(tool?.input)

  const act = async (d: 'allow' | 'deny') => {
    setBusy(d)
    try {
      await onResolve(b.toolCallId, d)
    } finally {
      setBusy(null)
    }
  }

  const label = () => {
    switch (b.decision) {
      case 'allow':
        return '已允许'
      case 'deny':
        return b.by === 'system' ? '随本轮中止' : '已拒绝'
      case 'timeout':
        return '超时未处理'
      default:
        return '已处理'
    }
  }

  // 已处理的审批收成一行。
  // 理由：审批是审计信息，每一步都留一张大卡片会把对话流冲散——
  // 用户真正要看的是 agent 干了什么，而不是每一条「已允许」。
  if (decided) {
    return (
      <div className="approval-line">
        <span className={b.decision === 'allow' ? 'ok' : 'no'}>
          {b.decision === 'allow' ? '✓' : '×'}
        </span>
        <span>{label()}</span>
        {kind && (
          <span className="tool-kind" style={{ transform: 'scale(.92)' }}>
            {KIND_LABEL[kind] || kind}
          </span>
        )}
        <code>{title}</code>
      </div>
    )
  }

  return (
    <div className="approval">
      <div className="approval-head">
        ⏸ 需要你确认
        {kind && (
          <span className="tool-kind" style={{ marginLeft: 'auto' }}>
            {KIND_LABEL[kind] || kind}
          </span>
        )}
      </div>
      <div className="approval-body">
        Agent 想{kind === 'execute' ? '执行命令' : kind === 'edit' ? '修改文件' : '执行操作'}：
        <code>{title}</code>
        {detail && detail !== title && <pre className="tool-pre" style={{ marginTop: 6 }}>{detail}</pre>}
        {b.policy && (
          <div style={{ marginTop: 4, fontSize: 11.5, color: 'var(--muted)' }}>
            策略 <code>{b.policy}</code> · 超时按拒绝处理
          </div>
        )}
      </div>
      <div className="approval-actions">
        <button className="btn allow" disabled={busy !== null} onClick={() => act('allow')}>
          {busy === 'allow' && <span className="spinner" />}允许执行
        </button>
        <button className="btn deny" disabled={busy !== null} onClick={() => act('deny')}>
          {busy === 'deny' && <span className="spinner" />}拒绝
        </button>
      </div>
    </div>
  )
}

function Thought({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="thought">
      <button className="thought-toggle" onClick={() => setOpen(!open)}>
        <span className={`caret ${open ? 'open' : ''}`}>▶</span>
        思考过程 · {text.replace(/\s/g, '').length} 字
      </button>
      {open && <div className="thought-text">{text}</div>}
    </div>
  )
}

function Blocks({
  blocks,
  running,
  onResolve,
  onCopy,
}: {
  blocks: Block[]
  running: boolean
  onResolve: (toolCallId: string, d: 'allow' | 'deny') => Promise<void>
  onCopy: (text: string) => void
}) {
  // 审批要按 toolCallId 回查对应的工具块：dsh 的权限请求只带 toolCallId。
  const byId = new Map<string, ToolBlock>()
  for (const b of blocks) if (b.type === 'tool') byId.set(b.id, b)

  return (
    <>
      {blocks.map((b, i) => {
        switch (b.type) {
          case 'message':
            return (
              <MsgRow
                key={i}
                kind="agent"
                body={
                  <div className="msg-assistant">
                    <div className="agent-tag">
                      <span className="sq" />
                      agent
                    </div>
                    <Markdown text={b.text} />
                    {running && i === blocks.length - 1 && <span className="streaming-caret" />}
                  </div>
                }
                actions={<MsgActions text={b.text} onCopy={onCopy} />}
              />
            )
          case 'thought':
            return <Thought key={i} text={b.text} />
          case 'tool':
            return <ToolCard key={i} b={b} />
          case 'approval':
            return (
              <ApprovalCard key={i} b={b} tool={byId.get(b.toolCallId)} onResolve={onResolve} />
            )
          case 'error':
            return (
              <div className="err" key={i}>
                {b.text}
              </div>
            )
          case 'log':
            return null
          default:
            return null
        }
      })}
    </>
  )
}

export function Turn({
  run,
  blocks,
  status,
  onResolve,
  onRetry,
  onCopy,
  onReuse,
}: {
  run: Run
  blocks: Block[]
  status: string
  onResolve: (toolCallId: string, d: 'allow' | 'deny') => Promise<void>
  onRetry: () => void
  onCopy: (text: string) => void
  onReuse: (text: string) => void
}) {
  const running = !isTerminal(status)
  const empty = blocks.length === 0

  return (
    <div className="turn">
      <MsgRow
        kind="user"
        body={<div className="msg-user">{run.input}</div>}
        actions={<MsgActions text={run.input} onCopy={onCopy} onReuse={onReuse} />}
      />

      {empty && running && (
        <div className="agent-tag">
          <span className="sq" />
          agent 启动中
          <span className="spinner" style={{ marginLeft: 4 }} />
        </div>
      )}

      <Blocks blocks={blocks} running={running} onResolve={onResolve} onCopy={onCopy} />

      {!running && status !== 'completed' && (
        <div className="agent-tag turn-end" style={{ marginTop: 2 }}>
          <span
            className="sq"
            style={{
              background: status === 'failed' ? 'var(--red)' : 'var(--muted)',
            }}
          />
          {STATUS_LABEL[status] || status}
          {run.stopReason ? ` · ${run.stopReason}` : ''}
          <button className="link-btn" onClick={onRetry} title="用同样的输入再发一次">
            重发
          </button>
        </div>
      )}
    </div>
  )
}
