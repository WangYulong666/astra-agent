// 会话搜索。
//
// 两种形态共用一份逻辑，只换外壳：
//   - 桌面（≥768）：Cmd/Ctrl+K 拉起的浮层，居中偏上，输入即过滤；
//   - 手机（<768）：顶栏放大镜点开的整屏层，输入框自动聚焦，右上「取消」返回。
//
// 为什么不做「一个浮层 + 媒体查询改尺寸」：手机上整屏层要吃掉安全区、要盖住
// 输入法，还要一个明确的返回入口——这些都是结构差异，只改 CSS 会写出一堆
// 互相抵消的覆盖规则。

import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import type { ReactNode } from 'react'
import { IconClose } from './icons'
import { IconPin, IconSearch } from './ui'

export type PaletteSession = {
  id: string
  title: string
  /** 最近一条指令。拿不到就是 undefined（还在读 / 会话里没有消息） */
  summary?: string
  pinned: boolean
  /** 当前打开的会话，列表里给个标记 */
  active: boolean
}

// ── 匹配 ────────────────────────────────────────────────────
//
// 不引模糊搜索库（fuse.js 之类）：这里只需要「大小写不敏感的子串匹配」，
// 加一条「标题命中优先于摘要命中」的排序规则。自己写 30 行的好处是排序
// 可解释——用户搜「部署」时，标题叫「部署脚本」的会话一定排在正文里
// 顺口提过一句部署的会话前面。

/** 命中位置：0 标题开头 / 1 标题中间 / 2 只有摘要命中 / null 不匹配 */
function rank(title: string, summary: string, q: string): number | null {
  const k = q.toLowerCase()
  const at = title.toLowerCase().indexOf(k)
  if (at === 0) return 0
  if (at > 0) return 1
  if (summary.toLowerCase().includes(k)) return 2
  return null
}

/** 摘要太长时只留命中位置附近的一段，免得整行都被挤掉。 */
function clip(text: string, q: string, max = 64): string {
  if (text.length <= max) return text
  const k = q.toLowerCase()
  const at = k ? text.toLowerCase().indexOf(k) : -1
  const from = Math.max(0, Math.min(at < 0 ? 0 : at - 16, text.length - max))
  const cut = text.slice(from, from + max)
  return `${from > 0 ? '…' : ''}${cut}${from + max < text.length ? '…' : ''}`
}

/** 命中片段包 <mark>。逐字符拼 ReactNode，绝不拼 innerHTML。 */
function Highlight({ text, query }: { text: string; query: string }): JSX.Element {
  const k = query.toLowerCase()
  if (!k) return <>{text}</>
  const lower = text.toLowerCase()
  const out: ReactNode[] = []
  let at = 0
  for (;;) {
    const i = lower.indexOf(k, at)
    if (i < 0) break
    if (i > at) out.push(text.slice(at, i))
    out.push(<mark key={`${i}-${out.length}`}>{text.slice(i, i + k.length)}</mark>)
    at = i + k.length
  }
  if (at < text.length) out.push(text.slice(at))
  return <>{out}</>
}

export function CommandPalette({
  open,
  full,
  sessions,
  onClose,
  onSelect,
}: {
  open: boolean
  /** 手机整屏形态 */
  full: boolean
  /** 已按「置顶 → 最近活跃」排好序，这里只叠加匹配相关度 */
  sessions: PaletteSession[]
  onClose: () => void
  onSelect: (id: string) => void
}) {
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const boxRef = useRef<HTMLDivElement>(null)
  const listRef = useRef<HTMLDivElement>(null)

  // 每次打开都从空白开始：上次搜什么不该影响这一次。
  useEffect(() => {
    if (!open) return
    setQ('')
    setSel(0)
    // autoFocus 在 portal 里偶尔会被后面的渲染抢走焦点，显式再要一次。
    const t = window.setTimeout(() => inputRef.current?.focus(), 20)
    return () => window.clearTimeout(t)
  }, [open])

  const hits = useMemo(() => {
    const query = q.trim()
    const rows: { s: PaletteSession; rank: number; order: number }[] = []
    sessions.forEach((s, i) => {
      const r = rank(s.title, s.summary ?? '', query)
      // 没有输入时 rank 视作 0：整份列表按传入顺序（置顶在前）展示。
      if (query && r === null) return
      rows.push({ s, rank: query ? (r as number) : 0, order: i })
    })
    // 置顶优先 → 命中位置（标题开头 > 标题中间 > 摘要） → 列表原顺序
    rows.sort((a, b) => {
      if (a.s.pinned !== b.s.pinned) return a.s.pinned ? -1 : 1
      if (a.rank !== b.rank) return a.rank - b.rank
      return a.order - b.order
    })
    return rows.map((r) => r.s)
  }, [q, sessions])

  // 过滤后条数变少时把选中项拉回范围内，否则 ↑↓ 会停在一个不存在的位置。
  useEffect(() => {
    setSel((cur) => (cur >= hits.length ? 0 : cur))
  }, [hits.length])

  // 选中项滚动进视野。scrollIntoView 在整屏层里会把外层也滚一下，
  // 所以只滚列表容器。
  useEffect(() => {
    const list = listRef.current
    const el = list?.querySelector<HTMLElement>(`[data-idx="${sel}"]`)
    if (!list || !el) return
    const top = el.offsetTop - list.offsetTop
    if (top < list.scrollTop) list.scrollTop = top
    else if (top + el.offsetHeight > list.scrollTop + list.clientHeight) {
      list.scrollTop = top + el.offsetHeight - list.clientHeight
    }
  }, [sel, hits])

  if (!open) return null

  const pick = (id: string) => {
    onSelect(id)
    onClose()
  }

  // 键盘：Esc 关、↑↓ 选、Enter 打开、Tab 只在面板内循环（焦点陷阱）。
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      e.stopPropagation()
      onClose()
      return
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      if (hits.length === 0) return
      e.preventDefault()
      setSel((cur) => {
        const next = e.key === 'ArrowDown' ? cur + 1 : cur - 1
        return (next + hits.length) % hits.length
      })
      return
    }
    if (e.key === 'Enter') {
      const hit = hits[sel]
      if (hit) {
        e.preventDefault()
        pick(hit.id)
      }
      return
    }
    if (e.key === 'Tab') {
      const root = boxRef.current
      if (!root) return
      const items = Array.from(
        root.querySelectorAll<HTMLElement>('button, input, [href], [tabindex]:not([tabindex="-1"])'),
      ).filter((el) => !el.hasAttribute('disabled'))
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      const cur = document.activeElement
      if (!e.shiftKey && cur === last) {
        e.preventDefault()
        first.focus()
      } else if (e.shiftKey && cur === first) {
        e.preventDefault()
        last.focus()
      }
    }
  }

  return createPortal(
    <div
      className={`palette-mask ${full ? 'full' : ''}`}
      // 点遮罩关闭用 mousedown：用 click 的话，从输入框里拖选文字松手
      // 落在遮罩上会被当成「点了外面」，搜索面板突然消失。
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        className={`palette ${full ? 'full' : ''}`}
        role="dialog"
        aria-modal="true"
        aria-label="搜索会话"
        ref={boxRef}
        onKeyDown={onKeyDown}
      >
        <div className="palette-search">
          <span className="palette-icon" aria-hidden="true">
            <IconSearch />
          </span>
          <input
            ref={inputRef}
            className="palette-input"
            value={q}
            type="search"
            enterKeyHint="go"
            placeholder="搜索会话标题或最近的消息…"
            aria-label="搜索会话"
            aria-controls="palette-results"
            aria-activedescendant={hits[sel] ? `palette-hit-${sel}` : undefined}
            onChange={(e) => {
              setQ(e.target.value)
              setSel(0)
            }}
          />
          {q && (
            <button
              className="palette-clear"
              onClick={() => {
                setQ('')
                inputRef.current?.focus()
              }}
              title="清空"
              aria-label="清空搜索"
            >
              <IconClose />
            </button>
          )}
          {full ? (
            <button className="palette-cancel" onClick={onClose}>
              取消
            </button>
          ) : (
            <kbd className="palette-kbd">Esc</kbd>
          )}
        </div>

        <div className="palette-results" id="palette-results" role="listbox" ref={listRef} aria-label="会话">
          {hits.length === 0 ? (
            <div className="palette-empty">
              {/* 一个会话都没有（新装的账号）和「搜不到」是两件事，
                  文案混用会让人以为搜索坏了。 */}
              <div className="palette-empty-title">
                {sessions.length === 0 ? '还没有任何会话' : '没有匹配的会话'}
              </div>
              <div className="palette-empty-hint">
                {sessions.length === 0
                  ? '从「新建会话」开始，agent 会在独立的沙箱里干活。'
                  : '换个关键词，或者新建一个会话来开始这次任务。'}
              </div>
            </div>
          ) : (
            hits.map((s, i) => (
              <button
                key={s.id}
                id={`palette-hit-${i}`}
                data-idx={i}
                role="option"
                aria-selected={i === sel}
                className={`palette-item ${i === sel ? 'sel' : ''}`}
                onMouseMove={() => i !== sel && setSel(i)}
                onClick={() => pick(s.id)}
              >
                <div className="palette-item-title">
                  {s.pinned && (
                    <span className="palette-pin" title="已置顶" aria-label="已置顶">
                      <IconPin filled />
                    </span>
                  )}
                  <span className="palette-item-text">
                    <Highlight text={s.title} query={q.trim()} />
                  </span>
                  {s.active && <span className="palette-item-cur">当前</span>}
                </div>
                {s.summary === undefined ? (
                  <div className="palette-item-sum loading">读取最近消息…</div>
                ) : s.summary ? (
                  <div className="palette-item-sum">
                    <Highlight text={clip(s.summary, q.trim())} query={q.trim()} />
                  </div>
                ) : (
                  <div className="palette-item-sum loading">还没有消息</div>
                )}
              </button>
            ))
          )}
        </div>

        {!full && (
          <div className="palette-foot">
            <span>
              <kbd>↑</kbd>
              <kbd>↓</kbd> 选择
            </span>
            <span>
              <kbd>Enter</kbd> 打开
            </span>
            <span>
              <kbd>Esc</kbd> 关闭
            </span>
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}
