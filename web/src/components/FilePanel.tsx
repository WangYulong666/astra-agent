import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { FileContent, FileEntry } from '../types'
import {
  DownloadError,
  classifyFile,
  fetchFileBlob,
  formatSize,
  insertIntoComposer,
  middleEllipsis,
  needsBigFileWarning,
  progressPercent,
  saveBlob,
  shareBlob,
  supportsFileShare,
} from '../files'
import { IconClose, IconFile, IconFolder, IconRefresh } from './icons'

// 图标就地定义：icons.tsx 是别人也在改的共享文件，为一个文件面板往里加十个
// export 只会制造冲突。这里的尺寸/线宽与 icons.tsx 保持一致。
const S = {
  width: 13,
  height: 13,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.7,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
}

const IcoDownload = () => (
  <svg {...S}>
    <path d="M12 3v12M7.5 10.5 12 15l4.5-4.5M4 20h16" />
  </svg>
)
const IcoShare = () => (
  <svg {...S}>
    <path d="M12 15V3M8 7l4-4 4 4M5 13v6a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-6" />
  </svg>
)
const IcoMore = () => (
  <svg {...S}>
    <circle cx="5.5" cy="12" r="1.1" fill="currentColor" stroke="none" />
    <circle cx="12" cy="12" r="1.1" fill="currentColor" stroke="none" />
    <circle cx="18.5" cy="12" r="1.1" fill="currentColor" stroke="none" />
  </svg>
)
const IcoCopy = () => (
  <svg {...S}>
    <rect x="9" y="9" width="11" height="12" rx="2" />
    <path d="M15 5.5A1.5 1.5 0 0 0 13.5 4H5a1 1 0 0 0-1 1v10.5A1.5 1.5 0 0 0 5.5 17" />
  </svg>
)
const IcoInsert = () => (
  <svg {...S}>
    <path d="M12 4v12M8 12l4 4 4-4M4 20h16" transform="rotate(180 12 12)" />
  </svg>
)
const IcoUp = () => (
  <svg {...S}>
    <path d="M12 19V5M6 11l6-6 6 6" />
  </svg>
)
const IcoImage = () => (
  <svg {...S} width={15} height={15}>
    <rect x="3" y="4" width="18" height="16" rx="2" />
    <circle cx="8.5" cy="9.5" r="1.6" />
    <path d="m21 16-5-5-6 6-2-2-5 5" />
  </svg>
)

/** 缩略图最多读这么多字节：再大就不是「缩略图」了，是替用户下载一遍。 */
const THUMB_MAX_BYTES = 12 * 1024 * 1024
/** 一个目录里最多渲染这么多张缩略图。 */
const THUMB_LIMIT = 30
/** 同时并发几个缩略图请求。太多会把控制面当 CDN 打。 */
const THUMB_CONCURRENCY = 3
/** 长按判定时长。短于这个数值会和「按住拖动列表」抢事件。 */
const LONG_PRESS_MS = 450

type Job = { loaded: number; total: number | null }
type MenuState = { entry: FileEntry; x: number; y: number }
type Lightbox = { name: string; path: string; url: string; size: number; owned: boolean }

export function FilePanel({
  sessionId,
  onClose,
}: {
  sessionId: string
  onClose: () => void
}) {
  const [path, setPath] = useState('')
  const [entries, setEntries] = useState<FileEntry[]>([])
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const [preview, setPreview] = useState<FileContent | null>(null)
  const [menu, setMenu] = useState<MenuState | null>(null)
  const [thumbs, setThumbs] = useState<Record<string, { url?: string; fail?: boolean }>>({})
  const [jobs, setJobs] = useState<Record<string, Job>>({})
  const [box, setBox] = useState<Lightbox | null>(null)
  const [toast, setToast] = useState('')
  const [shareable, setShareable] = useState(false)

  const pathBar = useRef<HTMLDivElement>(null)
  const toastTimer = useRef<number | undefined>(undefined)
  const longPress = useRef<number | undefined>(undefined)
  const pressOrigin = useRef({ x: 0, y: 0 })
  const suppressClick = useRef(false)

  const say = useCallback((msg: string) => {
    setToast(msg)
    if (toastTimer.current) window.clearTimeout(toastTimer.current)
    toastTimer.current = window.setTimeout(() => setToast(''), 2600)
  }, [])

  useEffect(() => {
    // 能力探测只做一次：navigator.canShare 是同步的，但每次都构造 File 没必要
    setShareable(supportsFileShare())
    return () => {
      if (toastTimer.current) window.clearTimeout(toastTimer.current)
    }
  }, [])

  const load = useCallback(
    async (p: string) => {
      setLoading(true)
      setErr('')
      try {
        const res = await api.listFiles(sessionId, p)
        setEntries(res.entries)
        setPath(res.path)
      } catch (e) {
        setErr((e as Error).message)
        setEntries([])
      } finally {
        setLoading(false)
      }
    },
    [sessionId],
  )

  useEffect(() => {
    void load('')
  }, [load])

  // ── 缩略图：只对图片、且只在面板打开期间持有 objectURL ──────────
  useEffect(() => {
    const images = entries
      .filter((e) => !e.isDir && classifyFile(e.name) === 'image' && e.size <= THUMB_MAX_BYTES)
      // 上限是怕「一个装了 200 张图的目录」变成 200 次全量下载：缩略图没有服务端
      // 缩放，每次都是一整个文件。前 30 张足够判断这批产出是什么。
      .slice(0, THUMB_LIMIT)
    setThumbs({})
    if (!images.length) return

    const created: string[] = []
    let cancelled = false
    let cursor = 0

    const worker = async () => {
      for (;;) {
        if (cancelled) return
        const f = images[cursor++]
        if (!f) return
        try {
          const { blob } = await fetchFileBlob(sessionId, f.path, {
            connectTimeoutMs: 8000,
            idleTimeoutMs: 8000,
          })
          if (cancelled) return
          const url = URL.createObjectURL(blob)
          created.push(url)
          setThumbs((prev) => ({ ...prev, [f.path]: { url } }))
        } catch {
          if (!cancelled) setThumbs((prev) => ({ ...prev, [f.path]: { fail: true } }))
        }
      }
    }

    void Promise.all(
      Array.from({ length: Math.min(THUMB_CONCURRENCY, images.length) }, () => worker()),
    )

    return () => {
      cancelled = true
      // 不撤销就是泄漏：一张 4MB 的图跟着页面活到刷新
      created.forEach((u) => URL.revokeObjectURL(u))
    }
  }, [entries, sessionId])

  // 目录一变就把面包屑滚到最右：当前目录被挤到视野外等于没导航
  useEffect(() => {
    const el = pathBar.current
    if (el) el.scrollLeft = el.scrollWidth
  }, [path])

  // 菜单：点空白 / Esc / 滚动都关掉。滚动也要关，否则菜单会停在原处指向别的文件。
  useEffect(() => {
    if (!menu) return
    // 点菜单内部不能关：window 上的捕获相监听比 React 的冒泡处理更早，
    // 无脑关闭会让「点菜单项」变成「先关菜单再点到空处」。
    const close = (e?: Event) => {
      const t = e?.target as HTMLElement | null
      if (t?.closest?.('.file-menu')) return
      setMenu(null)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMenu(null)
    }
    window.addEventListener('pointerdown', close, true)
    window.addEventListener('keydown', onKey)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    return () => {
      window.removeEventListener('pointerdown', close, true)
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [menu])

  // 灯箱：Esc 关闭。缩放交给浏览器（双指/滚轮），这里不接管。
  useEffect(() => {
    if (!box) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeBox()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
    // closeBox 用到了 box 本身，靠闭包拿最新值即可
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [box])

  const closeBox = () => {
    setBox((cur) => {
      if (cur?.owned) URL.revokeObjectURL(cur.url)
      return null
    })
  }

  // ── 下载 / 分享 ────────────────────────────────────────────
  const runJob = useCallback(
    async (f: FileEntry, mode: 'download' | 'share') => {
      if (needsBigFileWarning(f.size)) {
        const ok = window.confirm(
          `文件较大（${formatSize(f.size)}），手机上可能会很慢或失败，建议在桌面端处理。仍要继续吗？`,
        )
        if (!ok) return
      }
      setJobs((prev) => ({ ...prev, [f.path]: { loaded: 0, total: f.size || null } }))
      try {
        const { blob, filename } = await fetchFileBlob(sessionId, f.path, {
          onProgress: (loaded, total) =>
            setJobs((prev) => ({ ...prev, [f.path]: { loaded, total } })),
        })
        if (mode === 'share') {
          await shareBlob(blob, filename)
          say(`已打开分享：${filename}`)
        } else {
          saveBlob(blob, filename)
          say(`已保存 ${filename}（${formatSize(blob.size)}）`)
        }
      } catch (e) {
        // DownloadError 的 message 已经是可读文案（区分 401/404/网络）
        say(e instanceof DownloadError ? e.message : `下载失败：${(e as Error).message}`)
      } finally {
        setJobs((prev) => {
          const next = { ...prev }
          delete next[f.path]
          return next
        })
      }
    },
    [sessionId, say],
  )

  const copyPath = async (p: string) => {
    try {
      await navigator.clipboard.writeText(p)
      say('路径已复制')
    } catch {
      // 非安全上下文（http + 非 localhost）没有 clipboard API，退回到老办法
      const ta = document.createElement('textarea')
      ta.value = p
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      ta.remove()
      say(ok ? '路径已复制' : '复制失败，请手动选中路径')
    }
  }

  const insertPath = (p: string) => {
    if (!insertIntoComposer(p)) {
      say('输入框还没准备好，请先关掉文件面板')
      return
    }
    say('已插入到输入框')
    // 手机上输入框在面板后面，不关掉等于「点了没反应」
    if (window.innerWidth <= 1023) onClose()
  }

  // ── 打开 ───────────────────────────────────────────────────
  const openEntry = async (f: FileEntry) => {
    if (f.isDir) {
      void load(f.path)
      return
    }
    const kind = classifyFile(f.name)
    if (kind === 'image') {
      await openImage(f)
      return
    }
    // 二进制不读内容：读了也只会拿到一个 binary=true 的空壳，白等一次往返
    if (kind === 'binary') {
      setPreview({
        path: f.path,
        content: '',
        size: f.size,
        truncated: false,
        binary: true,
      })
      return
    }
    try {
      setPreview(await api.readFile(sessionId, f.path))
    } catch (e) {
      // 预览失败只弹提示，不占满整个面板：面板本身还是好的，用户可以继续下载
      say((e as Error).message)
    }
  }

  const openImage = async (f: FileEntry) => {
    const cached = thumbs[f.path]?.url
    if (cached) {
      setBox({ name: f.name, path: f.path, url: cached, size: f.size, owned: false })
      return
    }
    try {
      const { blob } = await fetchFileBlob(sessionId, f.path)
      const url = URL.createObjectURL(blob)
      setBox({ name: f.name, path: f.path, url, size: blob.size, owned: true })
    } catch (e) {
      say(e instanceof DownloadError ? e.message : '图片加载失败')
    }
  }

  // ── 菜单手势 ───────────────────────────────────────────────
  const openMenuAt = (f: FileEntry, x: number, y: number) => {
    // 贴边时往回夹，否则菜单会跑出视口
    const w = 172
    const h = shareable ? 176 : 142
    setMenu({
      entry: f,
      x: Math.max(8, Math.min(x, window.innerWidth - w - 8)),
      y: Math.max(8, Math.min(y, window.innerHeight - h - 8)),
    })
  }

  const onRowTouchStart = (f: FileEntry) => (e: React.TouchEvent) => {
    if (e.touches.length !== 1) return
    const t = e.touches[0]
    pressOrigin.current = { x: t.clientX, y: t.clientY }
    suppressClick.current = false
    if (longPress.current) window.clearTimeout(longPress.current)
    longPress.current = window.setTimeout(() => {
      suppressClick.current = true
      // 轻微振动是「长按生效了」的唯一反馈，不支持就算了
      navigator.vibrate?.(10)
      openMenuAt(f, pressOrigin.current.x, pressOrigin.current.y)
    }, LONG_PRESS_MS)
  }

  const onRowTouchMove = (e: React.TouchEvent) => {
    if (!longPress.current) return
    const t = e.touches[0]
    if (!t) return
    const dx = t.clientX - pressOrigin.current.x
    const dy = t.clientY - pressOrigin.current.y
    // 手指一动就说明用户是在滚列表，不是要菜单
    if (Math.hypot(dx, dy) > 10) {
      window.clearTimeout(longPress.current)
      longPress.current = undefined
    }
  }

  const onRowTouchEnd = () => {
    if (longPress.current) window.clearTimeout(longPress.current)
    longPress.current = undefined
  }

  const crumbs = path ? path.split('/') : []
  // 折叠阈值：超过 3 段就只留最后两段。实测 330px 的侧栏放不下 4 段中文目录名
  const crumbStart = crumbs.length > 3 ? crumbs.length - 2 : 0
  const fileCount = entries.filter((e) => !e.isDir).length
  const images = entries
    .filter((e) => !e.isDir && classifyFile(e.name) === 'image')
    .slice(0, THUMB_LIMIT)
  const busy = (p: string) => jobs[p]

  return (
    <>
      {/* 平板/手机的浮层遮罩：文件面板这时候是盖在会话上的，没有它
          底下的消息会从半透明面板后面透出来，看着像是渲染错位。
          宽屏三栏布局下 CSS 直接 display:none。 */}
      <div className="files-scrim" onClick={onClose} aria-hidden="true" />
      <aside className="files">
        <div className="files-head">
          <span className="files-title">工作区文件</span>
          <span className="files-count" title="当前目录下的文件数">
            {loading ? '…' : `${fileCount} 个文件`}
          </span>
          <button className="icon-btn" onClick={() => void load(path)} title="刷新">
            <IconRefresh />
          </button>
          <button className="icon-btn" onClick={onClose} title="收起">
            <IconClose />
          </button>
        </div>

        <div className="files-path" title={path ? `/${path}` : 'workspace'} ref={pathBar}>
          <button
            className="crumb-up"
            disabled={!path}
            onClick={() => void load(path.split('/').slice(0, -1).join('/'))}
            title="上一级"
          >
            <IcoUp />
          </button>
          <button className="crumb" onClick={() => void load('')}>
            workspace
          </button>
          {/* 层级超过 3 段就折叠中间：手机会把最后两段（当前目录）挤出屏幕，
              而「现在在哪」恰恰是唯一必须一直看得见的信息。 */}
          {crumbStart > 0 && (
            <span className="crumb-seg">
              <span className="crumb-sep">/</span>
              <button
                className="crumb crumb-more"
                title={`省略 ${crumbStart} 级：${crumbs.slice(0, crumbStart).join('/')}`}
                onClick={() => void load(crumbs.slice(0, crumbStart).join('/'))}
              >
                …
              </button>
            </span>
          )}
          {crumbs.slice(crumbStart).map((c, i) => {
            const idx = crumbStart + i
            return (
              <span key={idx} className="crumb-seg">
                <span className="crumb-sep">/</span>
                <button
                  className={idx === crumbs.length - 1 ? 'crumb current' : 'crumb'}
                  title={crumbs.slice(0, idx + 1).join('/')}
                  onClick={() => void load(crumbs.slice(0, idx + 1).join('/'))}
                >
                  {middleEllipsis(c, 16)}
                </button>
              </span>
            )
          })}
        </div>

        <div className="file-list">
          {loading && (
            <div className="files-skeleton">
              {[0, 1, 2].map((i) => (
                <div key={i} className="skeleton" style={{ width: `${72 - i * 12}%` }} />
              ))}
            </div>
          )}

          {!loading && err && (
            <div className="files-empty files-failed">
              <div>读不到这个目录：{err}</div>
              <button className="files-retry" onClick={() => void load(path)}>
                重试
              </button>
            </div>
          )}

          {!loading && !err && entries.length === 0 && (
            <div className="files-empty">
              这个目录是空的。
              <br />
              让 agent 写点东西，这里就会出现产出物。
            </div>
          )}

          {!loading && !err && images.length > 0 && (
            <div className="files-thumbs">
              <div className="files-label">图片 · {images.length}</div>
              <div className="thumb-grid">
                {images.map((f) => {
                  const t = thumbs[f.path]
                  return (
                    <button
                      key={f.path}
                      className="thumb"
                      onClick={() => void openImage(f)}
                      onContextMenu={(e) => {
                        e.preventDefault()
                        openMenuAt(f, e.clientX, e.clientY)
                      }}
                      title={f.path}
                    >
                      <span className="thumb-box">
                        {t?.url ? (
                          <img src={t.url} alt={f.name} loading="lazy" />
                        ) : t?.fail ? (
                          <IcoImage />
                        ) : (
                          <span className="thumb-loading" />
                        )}
                      </span>
                      <span className="thumb-name">{middleEllipsis(f.name, 16)}</span>
                    </button>
                  )
                })}
              </div>
            </div>
          )}

          {!loading && !err && entries.length > 0 && (
            <div className="files-label files-label-list">全部</div>
          )}

          {!loading &&
            !err &&
            entries.map((f) => {
              const job = busy(f.path)
              const pct = job ? progressPercent(job.loaded, job.total) : null
              const isImage = !f.isDir && classifyFile(f.name) === 'image'
              const thumbUrl = thumbs[f.path]?.url
              return (
                <div
                  key={f.path}
                  className={`file-row${job ? ' is-busy' : ''}`}
                  onClick={() => {
                    if (suppressClick.current) {
                      suppressClick.current = false
                      return
                    }
                    void openEntry(f)
                  }}
                  onContextMenu={(e) => {
                    if (f.isDir) return
                    e.preventDefault()
                    openMenuAt(f, e.clientX, e.clientY)
                  }}
                  onTouchStart={onRowTouchStart(f)}
                  onTouchMove={onRowTouchMove}
                  onTouchEnd={onRowTouchEnd}
                  onTouchCancel={onRowTouchEnd}
                  title={f.path}
                >
                  <span className="ico">
                    {f.isDir ? (
                      <IconFolder />
                    ) : thumbUrl ? (
                      <img className="ico-thumb" src={thumbUrl} alt="" />
                    ) : isImage ? (
                      <IcoImage />
                    ) : (
                      <IconFile />
                    )}
                  </span>
                  <span className="nm">{f.name}</span>
                  <span className="sz">
                    {job ? (pct === null ? '下载中…' : `${pct}%`) : f.isDir ? '目录' : formatSize(f.size)}
                  </span>
                  {!f.isDir && (
                    <span className="row-acts">
                      {shareable && (
                        <button
                          className="row-act"
                          title="分享"
                          aria-label={`分享 ${f.name}`}
                          onClick={(e) => {
                            e.stopPropagation()
                            void runJob(f, 'share')
                          }}
                        >
                          <IcoShare />
                        </button>
                      )}
                      <button
                        className="row-act"
                        title="下载"
                        aria-label={`下载 ${f.name}`}
                        onClick={(e) => {
                          e.stopPropagation()
                          void runJob(f, 'download')
                        }}
                      >
                        <IcoDownload />
                      </button>
                      <button
                        className="row-act row-more"
                        title="更多"
                        aria-label={`更多操作 ${f.name}`}
                        onClick={(e) => {
                          e.stopPropagation()
                          const r = (e.currentTarget as HTMLElement).getBoundingClientRect()
                          openMenuAt(f, r.right - 8, r.bottom + 4)
                        }}
                      >
                        <IcoMore />
                      </button>
                    </span>
                  )}
                  {job && (
                    <span
                      className={`file-bar${pct === null ? ' indeterminate' : ''}`}
                      style={pct === null ? undefined : { width: `${pct}%` }}
                    />
                  )}
                </div>
              )
            })}
        </div>

        {menu && (
          <div
            className="file-menu"
            style={{ left: menu.x, top: menu.y }}
            onPointerDown={(e) => e.stopPropagation()}
            role="menu"
          >
            <button role="menuitem" onClick={() => (setMenu(null), void runJob(menu.entry, 'download'))}>
              <IcoDownload />
              下载
            </button>
            {shareable && (
              <button role="menuitem" onClick={() => (setMenu(null), void runJob(menu.entry, 'share'))}>
                <IcoShare />
                分享
              </button>
            )}
            <button role="menuitem" onClick={() => (setMenu(null), void copyPath(menu.entry.path))}>
              <IcoCopy />
              复制路径
            </button>
            <button role="menuitem" onClick={() => (setMenu(null), insertPath(menu.entry.path))}>
              <IcoInsert />
              插入到输入框
            </button>
          </div>
        )}

        {preview && (
          <div className="preview" onClick={() => setPreview(null)}>
            <div className="preview-card" onClick={(e) => e.stopPropagation()}>
              <div className="preview-head">
                <span title={preview.path}>{middleEllipsis(preview.path, 42)}</span>
                <span style={{ color: 'var(--muted)' }}>
                  {formatSize(preview.size)}
                  {preview.truncated ? ' · 已截断' : ''}
                  {preview.binary ? ' · 二进制' : ''}
                </span>
                <button
                  className="preview-act"
                  title="下载"
                  onClick={() => {
                    const f = entries.find((e) => e.path === preview.path)
                    if (f) void runJob(f, 'download')
                  }}
                >
                  <IcoDownload />
                </button>
                {shareable && (
                  <button
                    className="preview-act"
                    title="分享"
                    onClick={() => {
                      const f = entries.find((e) => e.path === preview.path)
                      if (f) void runJob(f, 'share')
                    }}
                  >
                    <IcoShare />
                  </button>
                )}
                <button className="icon-btn preview-close" onClick={() => setPreview(null)}>
                  <IconClose />
                </button>
              </div>
              {preview.binary ? (
                <div className="preview-binary">
                  <IcoImage />
                  <div className="preview-binary-name" title={preview.path}>
                    {middleEllipsis(preview.path, 34)}
                  </div>
                  <div className="preview-binary-meta">
                    {formatSize(preview.size)} · 二进制文件，不在这里预览
                  </div>
                  <button
                    className="preview-dl"
                    onClick={() => {
                      const f = entries.find((e) => e.path === preview.path)
                      if (f) void runJob(f, 'download')
                    }}
                  >
                    <IcoDownload />
                    下载这个文件
                  </button>
                </div>
              ) : (
                <pre className="preview-body">{preview.content || '（空文件）'}</pre>
              )}
            </div>
          </div>
        )}

        {box && (
          <div className="lightbox" onClick={closeBox} role="dialog" aria-label={box.name}>
            <div className="lightbox-bar" onClick={(e) => e.stopPropagation()}>
              <span className="lightbox-name" title={box.path}>
                {middleEllipsis(box.name, 30)}
              </span>
              <span className="lightbox-size">{formatSize(box.size)}</span>
              {shareable && (
                <button
                  className="preview-act"
                  title="分享"
                  onClick={() => {
                    const f = entries.find((e) => e.path === box.path)
                    if (f) void runJob(f, 'share')
                  }}
                >
                  <IcoShare />
                </button>
              )}
              <button
                className="preview-act"
                title="下载"
                onClick={() => {
                  const f = entries.find((e) => e.path === box.path)
                  if (f) void runJob(f, 'download')
                }}
              >
                <IcoDownload />
              </button>
              <button className="icon-btn" title="关闭 (Esc)" onClick={closeBox}>
                <IconClose />
              </button>
            </div>
            <img
              className="lightbox-img"
              src={box.url}
              alt={box.name}
              onClick={(e) => e.stopPropagation()}
            />
          </div>
        )}

        {toast && <div className="toast files-toast">{toast}</div>}
      </aside>
    </>
  )
}
