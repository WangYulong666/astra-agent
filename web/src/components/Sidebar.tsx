import { useState } from 'react'
import type { Session } from '../types'
import { relTime, shortId } from '../format'
import { useAuth } from '../auth'
import { useRoute } from '../router'
import { IconClose, IconPencil, IconPlus, IconTrash } from './icons'
import { IconPin } from './ui'

export function Sidebar({
  sessions,
  activeId,
  creating,
  pinned,
  open,
  onClose,
  onSelect,
  onCreate,
  onDelete,
  onRename,
  onTogglePin,
}: {
  sessions: Session[]
  activeId: string | null
  creating: boolean
  /** 置顶的会话 id（存在 localStorage，不进后端） */
  pinned: string[]
  /** 窄屏抽屉是否展开。宽屏下这个值不起作用（.sidebar 一直是可见的） */
  open: boolean
  onClose: () => void
  onSelect: (id: string) => void
  onCreate: () => void
  onDelete: (id: string) => void
  onRename: (id: string, title: string) => void
  onTogglePin: (id: string) => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const path = useRoute()
  const { canAdmin } = useAuth()
  const inAdmin = path.startsWith('/admin')

  const startEdit = (s: Session) => {
    setEditing(s.id)
    setDraft(s.title || '')
  }

  const commit = () => {
    const t = draft.trim()
    if (editing && t) onRename(editing, t)
    setEditing(null)
  }

  return (
    <>
      {/* 抽屉遮罩。宽屏下 CSS 直接 display:none，不会挡住侧栏。 */}
      <div className={`scrim ${open ? 'open' : ''}`} onClick={onClose} aria-hidden="true" />

      <aside className={`sidebar ${open ? 'open' : ''}`}>
        {/* 抽屉里的主导航。窄屏（<768）顶栏放不下「工作台 / 管理后台」两个
            文字入口，收进这里；宽屏由 .topnav 承担，这块 CSS 是 display:none。 */}
        <nav className="sidebar-nav" aria-label="主导航">
          <a className={!inAdmin ? 'on' : ''} href="#/" onClick={onClose}>
            工作台
          </a>
          {canAdmin && (
            <a className={inAdmin ? 'on' : ''} href="#/admin/users" onClick={onClose}>
              管理后台
            </a>
          )}
        </nav>

        <div className="sidebar-head">
          <button className="btn-new" onClick={onCreate} disabled={creating}>
            {creating ? <span className="spinner" /> : <IconPlus />}
            {creating ? '正在起沙箱…' : '新建会话'}
          </button>
          {/* 只有抽屉形态需要这个按钮：宽屏能直接看到侧栏，关它没有意义 */}
          <button className="icon-btn sidebar-close" onClick={onClose} title="收起侧栏" aria-label="收起侧栏">
            <IconClose />
          </button>
        </div>

      <div className="session-list">
        {sessions.length === 0 && (
          <div className="files-empty" style={{ padding: '14px 12px' }}>
            还没有会话。每个会话会独占一个沙箱和一个 agent 进程。
          </div>
        )}
        {sessions.map((s) => {
          const isPinned = pinned.includes(s.id)
          return (
            <div
              key={s.id}
              className={`session-item ${s.id === activeId ? 'active' : ''} ${isPinned ? 'pinned' : ''}`}
              onClick={() => editing !== s.id && onSelect(s.id)}
            >
              {editing === s.id ? (
                <input
                  className="title-input"
                  autoFocus
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  onBlur={commit}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') commit()
                    if (e.key === 'Escape') setEditing(null)
                  }}
                  onClick={(e) => e.stopPropagation()}
                />
              ) : (
                <div className="session-title">
                  {/* 置顶标记：一个 10px 的图钉，不用整块底色——侧栏本身已经很密 */}
                  {isPinned && (
                    <span className="session-pin" title="已置顶" aria-label="已置顶">
                      <IconPin filled />
                    </span>
                  )}
                  <span className="session-title-text">{s.title || '未命名会话'}</span>
                </div>
              )}
              <div className="session-meta">
                <span>{relTime(s.lastActiveAt)}</span>
                <span style={{ opacity: 0.5 }}>·</span>
                <span style={{ fontFamily: 'var(--mono)', fontSize: 10 }}>
                  {shortId(s.id)}
                </span>
              </div>

              <div className="session-actions" onClick={(e) => e.stopPropagation()}>
                <button
                  className={`mini-btn ${isPinned ? 'on' : ''}`}
                  title={isPinned ? '取消置顶' : '置顶'}
                  aria-label={isPinned ? '取消置顶' : '置顶'}
                  aria-pressed={isPinned}
                  onClick={() => onTogglePin(s.id)}
                >
                  <IconPin filled={isPinned} />
                </button>
                <button className="mini-btn" title="重命名" onClick={() => startEdit(s)}>
                  <IconPencil />
                </button>
                <button
                  className="mini-btn danger"
                  title="删除会话（会销毁沙箱）"
                  onClick={() => {
                    if (confirm(`删除会话「${s.title || s.id}」？沙箱与全部事件都会被清掉。`)) {
                      onDelete(s.id)
                    }
                  }}
                >
                  <IconTrash />
                </button>
              </div>
            </div>
          )
        })}
      </div>

      <div className="sidebar-foot">
        <span>沙箱工作区</span>
        <code title={sessions.find((s) => s.id === activeId)?.cwd}>
          {sessions.find((s) => s.id === activeId)?.cwd || '—'}
        </code>
      </div>
    </aside>
    </>
  )
}
