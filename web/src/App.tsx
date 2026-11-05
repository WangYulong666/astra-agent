import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, subscribeEvents } from './api'
import type { Run, RunEvent, Session } from './types'
import { STATUS_LABEL, isTerminal } from './types'
import { buildBlocks, lastStatus } from './timeline'
import { Sidebar } from './components/Sidebar'
import { Composer } from './components/Composer'
import { FilePanel } from './components/FilePanel'
import { Turn } from './components/Turn'
import { TopBar } from './components/TopBar'
import { CommandPalette } from './components/CommandPalette'
import type { PaletteSession } from './components/CommandPalette'
import { IconFolder, IconRefresh } from './components/icons'
import { copyText } from './components/ui'

// 示例指令。原则：点一下就能跑、跑完看得出效果，且都在沙箱工作区里发生。
// 写成「你好」这类寒暄没有意义——那不叫演示能力，只是验证通路。
const EXAMPLES = [
  { k: '看看工作区', t: '列出当前工作区的文件并解释结构' },
  { k: '写个脚本', t: '写一个 Python 脚本统计这个目录的代码行数' },
  { k: '改改代码', t: '把 app.py 改成分页输出并跑一遍测试' },
]

const PIN_KEY = 'astra_pinned_sessions'

// 置顶只存在本机：它是「我这台设备上先看哪几个」的偏好，不是会话属性。
// 存服务端反而要加字段、加迁移、还要处理多端冲突，收益对不上成本。
function readPinned(): string[] {
  try {
    const raw = window.localStorage.getItem(PIN_KEY)
    if (!raw) return []
    const list: unknown = JSON.parse(raw)
    return Array.isArray(list) ? list.filter((x): x is string => typeof x === 'string') : []
  } catch {
    return []
  }
}

type Conn = 'idle' | 'live' | 'retry' | 'closed'

export default function App() {
  const [meta, setMeta] = useState<Record<string, string>>({})
  const [sessions, setSessions] = useState<Session[]>([])
  const [activeId, setActiveId] = useState<string | null>(null)
  const [runs, setRuns] = useState<Run[]>([])
  const [events, setEvents] = useState<Record<string, RunEvent[]>>({})
  const [creating, setCreating] = useState(false)
  const [sending, setSending] = useState(false)
  const [loadingSession, setLoadingSession] = useState(false)
  const [conn, setConn] = useState<Conn>('idle')
  // 浏览器自己就知道断网了，比等 SSE 超时快得多：用它把「网络断了」尽快说出来。
  const [netOffline, setNetOffline] = useState(
    () => typeof navigator !== 'undefined' && navigator.onLine === false,
  )
  // ?files=1 直接把文件面板打开：截图、演示、排查产出物时少点一次。
  const [showFiles, setShowFiles] = useState(
    () => new URLSearchParams(window.location.search).get('files') === '1',
  )
  const [toast, setToast] = useState('')
  // 首屏拉不到就说明控制面没起：给一条明确的横幅，别让人对着空列表猜。
  const [offline, setOffline] = useState(false)
  const [atBottom, setAtBottom] = useState(true)
  // 窄屏的会话抽屉。宽屏下 Sidebar 一直是静态列，这个状态不影响布局。
  const [menuOpen, setMenuOpen] = useState(false)
  // 会话搜索。桌面是 ⌘K 浮层，手机是顶栏放大镜点开的整屏层，同一份状态。
  const [searchOpen, setSearchOpen] = useState(false)
  const [mobile, setMobile] = useState(
    () => window.matchMedia('(max-width: 767px)').matches,
  )
  const [pinned, setPinned] = useState<string[]>(readPinned)
  // 搜索结果里的「最近一条消息」。按需拉、拉到就缓存，不在启动时打 N 个请求。
  const [summaries, setSummaries] = useState<Record<string, string>>({})
  const askedSummary = useRef<Set<string>>(new Set())

  const streams = useRef<Map<string, () => void>>(new Map())
  const skipLoad = useRef<string | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true)

  // 软键盘。dvh 只知道地址栏收放，不知道键盘——iOS 上键盘弹出时布局视口
  // 不变，输入区就被盖住了。这里把 visualViewport 的真实高度写成 --vvh，
  // 窄屏的 .app 用它当高度，键盘一弹整块界面就跟着缩。
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    const sync = () => {
      document.documentElement.style.setProperty('--vvh', `${Math.round(vv.height)}px`)
    }
    sync()
    vv.addEventListener('resize', sync)
    vv.addEventListener('scroll', sync)
    return () => {
      vv.removeEventListener('resize', sync)
      vv.removeEventListener('scroll', sync)
    }
  }, [])

  // 抽屉开着时按 Esc 收起（外接键盘 / iPad 的情况）
  useEffect(() => {
    if (!menuOpen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMenuOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [menuOpen])

  // 断点只在这里读一次，供 CommandPalette 决定浮层形态。
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 767px)')
    const on = () => setMobile(mq.matches)
    on()
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])

  useEffect(() => {
    try {
      window.localStorage.setItem(PIN_KEY, JSON.stringify(pinned))
    } catch {
      /* 无痕模式写不进去，置顶只在本次会话内有效——不值得弹提示 */
    }
  }, [pinned])

  const pushToast = useCallback((msg: string) => {
    setToast(msg)
    window.setTimeout(() => setToast((cur) => (cur === msg ? '' : cur)), 4000)
  }, [])

  // ── 事件流 ──────────────────────────────────────────────

  const closeStream = useCallback((runId: string) => {
    const close = streams.current.get(runId)
    if (close) {
      close()
      streams.current.delete(runId)
    }
  }, [])

  const closeAllStreams = useCallback(() => {
    for (const close of streams.current.values()) close()
    streams.current.clear()
  }, [])

  const attach = useCallback(
    (runId: string) => {
      if (streams.current.has(runId)) return
      const close = subscribeEvents(runId, 0, {
        onEvent: (ev) => {
          setEvents((prev) => {
            const list = prev[runId] ?? []
            if (list.some((e) => e.seq === ev.seq)) return prev
            return { ...prev, [runId]: [...list, ev] }
          })
          if (ev.type === 'run_state') {
            const st = String(ev.payload?.status ?? '')
            if (st) {
              setRuns((prev) =>
                prev.map((r) =>
                  r.id === runId
                    ? { ...r, status: st as Run['status'], stopReason: ev.payload?.stopReason }
                    : r,
                ),
              )
              if (isTerminal(st)) {
                closeStream(runId)
                setConn('idle')
              }
            }
          }
        },
        // 服务端在缓冲满时会主动断开让我们续传；subscribeEvents 内部会带
        // after_seq 重开，这里不需要做任何事——丢的是通知，不是数据。
        onOverflow: () => undefined,
        onState: (s) => setConn(s),
      })
      streams.current.set(runId, close)
    },
    [closeStream],
  )

  // ── 会话 ────────────────────────────────────────────────

  const refreshSessions = useCallback(async () => {
    try {
      // 存储返回的是目录顺序（id 是随机 hex，等于乱序）。按最近活跃排，
      // 否则「进来默认选中第一个」会变成随机挑一个会话。
      const list = (await api.listSessions()).sort((a, b) =>
        b.lastActiveAt.localeCompare(a.lastActiveAt),
      )
      setSessions(list)
      setOffline(false)
    } catch {
      setOffline(true)
    }
  }, [])

  const loadSession = useCallback(
    async (id: string) => {
      closeAllStreams()
      setConn('idle')
      setLoadingSession(true)
      setEvents({})
      setRuns([])
      try {
        const rs = (await api.listRuns(id)).slice().sort((a, b) =>
          a.createdAt.localeCompare(b.createdAt),
        )
        const map: Record<string, RunEvent[]> = {}
        await Promise.all(
          rs.map(async (r) => {
            try {
              map[r.id] = await api.listEvents(r.id)
            } catch {
              map[r.id] = []
            }
          }),
        )
        setRuns(rs)
        setEvents(map)
        setLoadingSession(false)
        // 只有还在跑的 run 需要挂流；已结束的历史已经在 map 里了。
        for (const r of rs) {
          if (!isTerminal(r.status)) attach(r.id)
        }
      } catch (e) {
        setLoadingSession(false)
        pushToast(`打开会话失败：${(e as Error).message}`)
      }
    },
    [attach, closeAllStreams, pushToast],
  )

  useEffect(() => {
    void (async () => {
      try {
        setMeta(await api.meta())
      } catch {
        /* meta 只用于展示，取不到不影响使用 */
      }
      await refreshSessions()
    })()
  }, [refreshSessions])

  useEffect(() => {
    if (!activeId) return
    if (skipLoad.current === activeId) {
      skipLoad.current = null
      return
    }
    void loadSession(activeId)
  }, [activeId, loadSession])

  useEffect(() => () => closeAllStreams(), [closeAllStreams])

  // 首次挂载时自动选中最近一次会话，省掉「进来先点一下」这一步。
  useEffect(() => {
    if (!activeId && sessions.length > 0) setActiveId(sessions[0].id)
  }, [sessions, activeId])

  const createSession = useCallback(async (): Promise<Session | null> => {
    setCreating(true)
    try {
      const s = await api.createSession()
      setSessions((prev) => [s, ...prev])
      skipLoad.current = s.id
      setActiveId(s.id)
      setRuns([])
      setEvents({})
      closeAllStreams()
      setConn('idle')
      return s
    } catch (e) {
      pushToast(`起沙箱失败：${(e as Error).message}`)
      return null
    } finally {
      setCreating(false)
    }
  }, [closeAllStreams, pushToast])

  const deleteSession = useCallback(
    async (id: string) => {
      try {
        closeStream(id)
        await api.deleteSession(id)
        setSessions((prev) => prev.filter((s) => s.id !== id))
        // 删掉会话时顺手摘掉置顶，否则 localStorage 里会攒一堆死 id。
        setPinned((prev) => prev.filter((x) => x !== id))
        if (activeId === id) {
          setActiveId(null)
          setRuns([])
          setEvents({})
        }
      } catch (e) {
        pushToast(`删除失败：${(e as Error).message}`)
      }
    },
    [activeId, closeStream, pushToast],
  )

  const renameSession = useCallback(
    async (id: string, title: string) => {
      setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, title } : s)))
      try {
        await api.renameSession(id, title)
      } catch (e) {
        pushToast(`重命名失败：${(e as Error).message}`)
      }
    },
    [pushToast],
  )

  const togglePin = useCallback((id: string) => {
    setPinned((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]))
  }, [])

  // 置顶的排最前，其余按最近活跃。搜索面板用的是同一份顺序。
  const orderedSessions = useMemo(() => {
    return sessions.slice().sort((a, b) => {
      const pa = pinned.includes(a.id) ? 0 : 1
      const pb = pinned.includes(b.id) ? 0 : 1
      if (pa !== pb) return pa - pb
      return b.lastActiveAt.localeCompare(a.lastActiveAt)
    })
  }, [sessions, pinned])

  const paletteItems = useMemo<PaletteSession[]>(
    () =>
      orderedSessions.map((s) => ({
        id: s.id,
        title: s.title || '未命名会话',
        // undefined = 还没读；'' = 读过，但会话里没有消息
        summary: summaries[s.id],
        pinned: pinned.includes(s.id),
        active: s.id === activeId,
      })),
    [orderedSessions, summaries, pinned, activeId],
  )

  // 搜索面板打开时补摘要。只补缺的，拉到的结果留在状态里，反复开关不重复请求。
  useEffect(() => {
    if (!searchOpen) return
    for (const s of sessions) {
      if (s.id in summaries || askedSummary.current.has(s.id)) continue
      askedSummary.current.add(s.id)
      void (async () => {
        try {
          const rs = await api.listRuns(s.id)
          const last = rs
            .slice()
            .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
            .pop()
          setSummaries((prev) => ({ ...prev, [s.id]: last?.input ?? '' }))
        } catch {
          setSummaries((prev) => ({ ...prev, [s.id]: '' }))
        }
      })()
    }
  }, [searchOpen, sessions, summaries])

  // ⌘K / Ctrl+K 开搜索，`/` 也开（后者是终端式习惯，单手也能触发）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault()
        setSearchOpen((v) => !v)
        return
      }
      if (e.key === '/' && !e.metaKey && !e.ctrlKey && !e.altKey) {
        const el = e.target as HTMLElement | null
        const tag = el?.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || el?.isContentEditable) return
        e.preventDefault()
        setSearchOpen(true)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  // ── 发送 / 停止 / 审批 ──────────────────────────────────

  const send = useCallback(
    async (text: string) => {
      setSending(true)
      try {
        let sid = activeId
        if (!sid) {
          const s = await createSession()
          if (!s) return
          sid = s.id
        }
        const run = await api.startRun(sid, text)
        setRuns((prev) => [...prev, run])
        setEvents((prev) => ({ ...prev, [run.id]: [] }))
        attach(run.id)
        stickToBottom.current = true
        // 后端会拿第一条指令自动起标题，刷新一下列表就能看到。
        void refreshSessions()
      } catch (e) {
        pushToast(`发送失败：${(e as Error).message}`)
      } finally {
        setSending(false)
      }
    },
    [activeId, attach, createSession, pushToast, refreshSessions],
  )

  const activeRun = useMemo(() => {
    for (let i = runs.length - 1; i >= 0; i--) {
      if (!isTerminal(runs[i].status)) return runs[i]
    }
    return runs[runs.length - 1]
  }, [runs])

  const hasLiveRun = runs.some((r) => !isTerminal(r.status))

  const cancel = useCallback(async () => {
    const target = runs.slice().reverse().find((r) => !isTerminal(r.status))
    if (!target) return
    try {
      await api.cancelRun(target.id)
    } catch (e) {
      pushToast(`停止失败：${(e as Error).message}`)
    }
  }, [runs, pushToast])

  const resolve = useCallback(
    async (runId: string, toolCallId: string, decision: 'allow' | 'deny') => {
      try {
        await api.resolveApproval(runId, toolCallId, decision)
      } catch (e) {
        pushToast(`审批失败：${(e as Error).message}`)
      }
    },
    [pushToast],
  )

  // ── 消息操作 ────────────────────────────────────────────

  const copyMessage = useCallback(
    async (text: string) => {
      const ok = await copyText(text)
      pushToast(ok ? '已复制到剪贴板' : '浏览器不允许自动复制，请长按选中后复制')
    },
    [pushToast],
  )

  // 把一段文字填回输入框（不发送）。
  //
  // Composer 是受控组件且不在本轮的可改范围内，所以只能从 DOM 侧写值：
  // 直接 el.value = text 会被 React 的 value tracker 判成「没变化」而吞掉，
  // 必须走原型上的原生 setter 再补一个 input 事件，onChange 才会真的收到。
  const fillComposer = useCallback(
    (text: string) => {
      const el = document.querySelector<HTMLTextAreaElement>('.composer textarea')
      if (!el) {
        pushToast('还没找到输入框，稍后再试')
        return
      }
      const setter = Object.getOwnPropertyDescriptor(
        HTMLTextAreaElement.prototype,
        'value',
      )?.set
      setter?.call(el, text)
      el.dispatchEvent(new Event('input', { bubbles: true }))
      el.focus()
      el.setSelectionRange(text.length, text.length)
      el.scrollIntoView({ block: 'nearest' })
      pushToast('已填入输入框，确认后再发送')
    },
    [pushToast],
  )

  // ── 连接状态 ────────────────────────────────────────────

  // 手动重连：把还在跑的 run 的流全部重挂一遍。没有活着的 run 时退化成
  // 「重新拉一次会话列表」——至少能确认控制面还在。
  const reconnect = useCallback(() => {
    closeAllStreams()
    setConn('idle')
    const alive = runs.filter((r) => !isTerminal(r.status))
    for (const r of alive) attach(r.id)
    void refreshSessions()
    pushToast(alive.length > 0 ? '正在重连事件流…' : '已重新同步会话列表')
  }, [attach, closeAllStreams, refreshSessions, runs, pushToast])

  useEffect(() => {
    const off = () => setNetOffline(true)
    // 网络回来了：不用等 SSE 的重连计时器，立刻重挂一遍。
    const on = () => {
      setNetOffline(false)
      reconnect()
    }
    window.addEventListener('offline', off)
    window.addEventListener('online', on)
    return () => {
      window.removeEventListener('offline', off)
      window.removeEventListener('online', on)
    }
  }, [reconnect])

  // warn：重连中（琥珀）；bad：已经断开、需要手动介入（红）。
  // 'closed' 本身是「我们主动关流」，只有还有 run 在跑时才算是异常。
  const connAlert: 'warn' | 'bad' | null = netOffline
    ? 'warn'
    : conn === 'retry'
      ? 'warn'
      : conn === 'closed' && hasLiveRun
        ? 'bad'
        : null

  // ── 滚动 ────────────────────────────────────────────────

  useEffect(() => {
    const el = scrollRef.current
    if (!el || !stickToBottom.current) return
    el.scrollTop = el.scrollHeight
  }, [events, runs])

  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 120
    stickToBottom.current = near
    setAtBottom(near)
  }

  const scrollToBottom = () => {
    const el = scrollRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    stickToBottom.current = true
    setAtBottom(true)
  }

  const activeSession = sessions.find((s) => s.id === activeId)
  const running = Boolean(activeRun && !isTerminal(activeRun.status))
  const status = activeRun?.status ?? ''
  const modKey = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent) ? '⌘' : 'Ctrl'

  return (
    <div className={`app ${showFiles && activeId ? 'with-files' : ''}`}>
      {/* 顶栏抽到 TopBar：工作台和管理后台共用同一份导航与用户菜单。
          这里只把工作台专属的运行信息（chips）和操作（连接状态、文件面板）插进去。 */}
      <TopBar
        chips={
          <>
            {meta.provider && (
              <span className="chip">
                <b>沙箱</b>
                {meta.provider}
              </span>
            )}
            {meta.policy && (
              <span className={`chip ${meta.policy === 'ask' ? 'warn' : ''}`}>
                <b>权限</b>
                {meta.approval === 'escalation'
                  ? '提权时审批'
                  : meta.policy === 'ask'
                    ? '逐条审批'
                    : meta.policy}
              </span>
            )}
            {meta.agent && (
              <span className="chip" title={meta.agent}>
                <b>agent</b>
                {meta.agentName || meta.agent.split('/').pop()}
              </span>
            )}
          </>
        }
        onMenu={() => setMenuOpen(true)}
        onSearch={() => setSearchOpen(true)}
        actions={
          <>
            {/* 手机：顶栏内联一个圆点药丸，不占内容高度、不挤 52px 顶栏，
                点一下就是重连（比「2px 横条 + 二级展开」少一次操作）。
                桌面：下面 main 里的细状态条，信息完整且不打断视线。 */}
            {connAlert && <ConnPill level={connAlert} onReconnect={reconnect} />}
            <button
              className={`icon-btn ${showFiles ? 'on' : ''}`}
              onClick={() => setShowFiles((v) => !v)}
              title="工作区文件"
              disabled={!activeId}
            >
              <IconFolder />
            </button>
          </>
        }
      />

      <Sidebar
        sessions={orderedSessions}
        activeId={activeId}
        creating={creating}
        pinned={pinned}
        open={menuOpen}
        onClose={() => setMenuOpen(false)}
        onSelect={(id) => {
          setActiveId(id)
          // 选完就收起抽屉：手机上留着它只会挡住刚打开的对话。
          setMenuOpen(false)
        }}
        onCreate={() => {
          setMenuOpen(false)
          void createSession()
        }}
        onDelete={(id) => void deleteSession(id)}
        onRename={(id, t) => void renameSession(id, t)}
        onTogglePin={togglePin}
      />

      <main className="main">
        {/* live 时整条不渲染：常驻一条绿线只会变成视觉噪音。 */}
        {connAlert && <ConnBar level={connAlert} onReconnect={reconnect} />}

        {offline && (
          <div className="banner">
            连不上控制面（127.0.0.1:7788）。
            <button className="link-btn" onClick={() => void refreshSessions()}>
              重试
            </button>
          </div>
        )}
        <div className="main-head">
          <span className="main-title">{activeSession?.title || '新会话'}</span>
          {status && (
            <span className={`pill ${status}`}>
              {status === 'running' && <span className="spinner" />}
              {STATUS_LABEL[status] || status}
            </span>
          )}
          <span className="head-spacer" />
          {activeSession && (
            <span className="pill sandbox-pill" title={activeSession.cwd}>
              沙箱 {activeSession.id.slice(0, 6)}
            </span>
          )}
        </div>

        <div className="stream" ref={scrollRef} onScroll={onScroll}>
          <div className="thread">
            {!activeId && (
              <div className="empty">
                <h1>把任务交给沙箱里的 agent</h1>
                <p>
                  每个会话独占一个沙箱和一个 agent 进程。命令、文件、审批记录都留在服务端，
                  关掉页面再回来还在。
                </p>
                <button className="btn allow" style={{ height: 36, padding: '0 20px' }} onClick={() => void createSession()}>
                  {creating ? '正在起沙箱…' : '新建第一个会话'}
                </button>
                <div className="examples">
                  {EXAMPLES.map((e) => (
                    <button key={e.k} className="example" onClick={() => fillComposer(e.t)}>
                      <span className="k">{e.k}</span>
                      {e.t}
                    </button>
                  ))}
                </div>
                <p className="empty-hint">
                  <kbd>{modKey}K</kbd> 搜索会话
                </p>
              </div>
            )}

            {activeId && loadingSession && (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                <div className="skeleton" style={{ width: '40%', marginLeft: 'auto' }} />
                <div className="skeleton" style={{ width: '86%' }} />
                <div className="skeleton" style={{ width: '62%' }} />
              </div>
            )}

            {activeId && !loadingSession && runs.length === 0 && (
              <div className="empty" style={{ minHeight: '46vh' }}>
                <h1 style={{ fontSize: 20 }}>这个会话还没有任务</h1>
                <p>直接在下面输入，或者从这些开始（点了只填进输入框，不会立刻发送）：</p>
                <div className="examples">
                  {EXAMPLES.map((e) => (
                    <button key={e.k} className="example" onClick={() => fillComposer(e.t)}>
                      <span className="k">{e.k}</span>
                      {e.t}
                    </button>
                  ))}
                </div>
                <p className="empty-hint">
                  <kbd>{modKey}K</kbd> 搜索会话
                </p>
              </div>
            )}

            {runs.map((r) => (
              <Turn
                key={r.id}
                run={r}
                status={lastStatus(events[r.id] ?? [], r.status)}
                blocks={buildBlocks(events[r.id] ?? [])}
                onResolve={(toolCallId, d) => resolve(r.id, toolCallId, d)}
                onRetry={() => void send(r.input)}
                onCopy={(t) => void copyMessage(t)}
                onReuse={fillComposer}
              />
            ))}
          </div>
        </div>

        {!atBottom && runs.length > 0 && (
          <button className="jump-bottom" onClick={scrollToBottom}>
            回到最新 ↓
          </button>
        )}

        <Composer
          running={running}
          disabled={sending || creating}
          onSend={(t) => void send(t)}
          onStop={() => void cancel()}
        />
      </main>

      {showFiles && activeId && (
        <FilePanel sessionId={activeId} onClose={() => setShowFiles(false)} />
      )}

      <CommandPalette
        open={searchOpen}
        full={mobile}
        sessions={paletteItems}
        onClose={() => setSearchOpen(false)}
        onSelect={(id) => {
          setActiveId(id)
          setMenuOpen(false)
        }}
      />

      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}

/**
 * 顶部细状态条（桌面）。只说一件事：现在能不能收到 agent 的消息。
 * live 时不渲染——正常状态不该占一行高度。
 */
function ConnBar({
  level,
  onReconnect,
}: {
  level: 'warn' | 'bad'
  onReconnect: () => void
}) {
  return (
    <div className={`conn-bar ${level}`} role="status" aria-live="polite">
      <span className="conn-bar-track" aria-hidden="true">
        <span className="conn-bar-fill" />
      </span>
      <span className="conn-bar-text">
        {level === 'bad' ? '已断开，消息不会再更新' : '连接中断，正在重连…'}
      </span>
      {level === 'bad' && (
        <button className="conn-bar-btn" onClick={onReconnect}>
          <IconRefresh />
          重连
        </button>
      )}
    </div>
  )
}

/**
 * 顶栏圆点药丸（窄屏）。选它而不是「2px 横条」的理由：
 * 窄屏顶栏本来就紧（320 上五个元素刚好排满），横条要么挤掉标题，要么
 * 压在消息流顶部挡住第一条消息的上边距；而在 52px 的顶栏里放一个
 * 44px 高的药丸，高度是现成的、命中区是够的、位置也不覆盖任何正文。
 */
function ConnPill({
  level,
  onReconnect,
}: {
  level: 'warn' | 'bad'
  onReconnect: () => void
}) {
  return (
    <button
      className={`conn-pill ${level}`}
      onClick={onReconnect}
      title={level === 'bad' ? '连接已断开，点击重连' : '连接中断，正在重连（点击立即重试）'}
      aria-label={level === 'bad' ? '连接已断开，点击重连' : '连接中断，正在重连'}
    >
      <span className="conn-pill-dot" aria-hidden="true" />
      <span className="conn-pill-text">{level === 'bad' ? '已断开' : '重连中'}</span>
    </button>
  )
}
