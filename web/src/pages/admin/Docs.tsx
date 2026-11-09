// 文档页（/admin/docs）。
//
// 三个容易被做坏的地方，写在最前面：
//
// 1. **503 不画空树。** 后端在 Docs 未装配时回 503 + {"error":…}。空目录
//    会被读成「文档还没写」，而真正的原因是 docs/ 没随二进制分发。这一屏
//    只回答两件事：现在没开、怎么开。
// 2. **404 不是「未启用」。** 路径手误、文档被删都会回 404，必须把服务端
//    的原话显示出来（isDocsDisabled 只认 503）。
// 3. **默认打开功能总览。** 它是运行时从能力注册表 + /v1/meta 生成的，
//    回答的是「这个平台现在到底有什么能力」——文档页的入口就该是它。
//
// 正文渲染交给 markdown.ts：那里先整体转义再按白名单拼标签，所以这里
// 可以放心使用 dangerouslySetInnerHTML。

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { docApi } from '../../api'
import type { DocDetail, DocMeta } from '../../api'
import { fmtTime } from '../../format'
import { Notice } from '../../components/ui'
import { renderMarkdown } from '../../markdown'
import {
  categoryLabel,
  docTitle,
  groupDocs,
  highlightSegments,
  isDocsDisabled,
  pickDefaultPath,
  stripLeadingH1,
} from './docsModel'

const SEARCH_DEBOUNCE_MS = 220

export function Docs() {
  // 目录
  const [items, setItems] = useState<DocMeta[] | null>(null)
  const [indexErr, setIndexErr] = useState('')
  const [disabled, setDisabled] = useState(false)

  // 当前文档
  const [path, setPath] = useState('')
  const [doc, setDoc] = useState<DocDetail | null>(null)
  const [docErr, setDocErr] = useState('')
  const [docLoading, setDocLoading] = useState(false)
  const [docReload, setDocReload] = useState(0)
  const docSeq = useRef(0)

  // 搜索
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<DocMeta[] | null>(null)
  const [searchErr, setSearchErr] = useState('')
  const [searchBusy, setSearchBusy] = useState(false)
  const searchSeq = useRef(0)

  const loadIndex = useCallback(async () => {
    setIndexErr('')
    setDisabled(false)
    setItems(null)
    try {
      const res = await docApi.index()
      const list = res.items || []
      setItems(list)
      // 保留当前选择（刷新后不要跳回总览）；路径不在了才回落到默认。
      setPath((cur) => (cur && list.some((d) => d.path === cur) ? cur : pickDefaultPath(list)))
    } catch (e) {
      // 503 = 模块没启用；其余（404/500/网络不通）都是真实错误，原样显示。
      if (isDocsDisabled(e)) setDisabled(true)
      else setIndexErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void loadIndex()
  }, [loadIndex])

  // 选中文档后取全文。用递增序号丢弃「切换太快导致的乱序响应」。
  useEffect(() => {
    if (!path) {
      setDoc(null)
      return
    }
    const mine = ++docSeq.current
    setDocLoading(true)
    setDocErr('')
    setDoc(null)
    docApi
      .get(path)
      .then((d) => {
        if (mine !== docSeq.current) return
        setDoc(d)
        setDocLoading(false)
      })
      .catch((e) => {
        if (mine !== docSeq.current) return
        setDocLoading(false)
        if (isDocsDisabled(e)) setDisabled(true)
        else setDocErr((e as Error).message)
      })
  }, [path, docReload])

  const term = query.trim()

  // 搜索：输入停 220ms 再打接口，避免每敲一个字都发请求。
  useEffect(() => {
    if (!term) {
      setResults(null)
      setSearchErr('')
      setSearchBusy(false)
      return
    }
    const mine = ++searchSeq.current
    setSearchBusy(true)
    const timer = window.setTimeout(() => {
      docApi
        .search(term, 20)
        .then((res) => {
          if (mine !== searchSeq.current) return
          setResults(res.items || [])
          setSearchErr('')
          setSearchBusy(false)
        })
        .catch((e) => {
          if (mine !== searchSeq.current) return
          setResults([])
          setSearchErr((e as Error).message)
          setSearchBusy(false)
        })
    }, SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [term])

  const groups = useMemo(() => groupDocs(items || []), [items])

  const bodyHtml = useMemo(() => {
    if (!doc) return ''
    // 页面顶部已经显示标题，正文里重复的一级标题先去掉。
    return renderMarkdown(stripLeadingH1(doc.body || '', doc.title || ''))
  }, [doc])

  const reloadAll = () => {
    void loadIndex()
    setDocReload((n) => n + 1)
  }

  const searching = term !== ''
  // Go 的零值时间经 JSON 出来是 0001-01-01…，fmtTime 会回 '—'；这时整条不显示，
  // 比写「更新于 —」诚实。
  const updated = doc ? fmtTime(doc.updatedAt) : '—'

  return (
    <div className="page docs-page">
      <div className="page-head">
        <div>
          <div className="page-title">文档</div>
          <div className="page-sub">
            产品内置文档，离线可读。功能总览由能力注册表与 /v1/meta 在请求时生成，
            其余来自 docs/（<code>make build</code> 会把 docs 拷进 bin/，启动时同步到 dataDir/docs）。
          </div>
        </div>
        <button className="btn page-act" onClick={reloadAll}>
          刷新
        </button>
      </div>

      {/* 搜索框始终在：未启用时它也解释不了什么，但保持位置稳定，
          避免「有文档 / 没文档」两套页头来回跳。 */}
      <div className="docs-tools">
        <div className="docs-search">
          <input
            className="input"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="搜索标题 / 摘要 / 正文"
            spellCheck={false}
            aria-label="搜索文档"
          />
          {query && (
            <button
              className="docs-clear"
              onClick={() => setQuery('')}
              title="清空搜索"
              aria-label="清空搜索"
              type="button"
            >
              ×
            </button>
          )}
        </div>
        {searching && (
          <span className="dim small">
            {searchBusy ? '搜索中…' : results ? `${results.length} 条结果` : ''}
          </span>
        )}
        <span className="docs-spacer" />
        {items && (
          <span className="dim small docs-count">
            共 {items.length} 篇 · {groups.length} 类
          </span>
        )}
      </div>

      {disabled ? (
        <DocsDisabled onRetry={() => void loadIndex()} />
      ) : indexErr ? (
        <Notice kind="error">
          读取文档目录失败：{indexErr}
          <div className="notice-sub">
            <button className="btn small" onClick={() => void loadIndex()}>
              重试
            </button>
          </div>
        </Notice>
      ) : items === null ? (
        <div className="docs-shell">
          <div className="docs-nav">
            <Skel rows={8} />
          </div>
          <div className="docs-doc">
            <div className="docs-doc-head">
              <div className="skeleton" style={{ height: 22, width: '46%' }} />
              <div className="skeleton" style={{ height: 12, width: '30%' }} />
            </div>
            <Skel rows={9} />
          </div>
        </div>
      ) : items.length === 0 ? (
        <div className="card">
          <div className="card-body">
            <div className="docs-empty">
              文档目录是空的：docs/ 里没有可读的 Markdown。功能总览由服务端生成，
              它也没出现说明生成源（能力注册表）当前不可用。
            </div>
          </div>
        </div>
      ) : (
        <>
          {/* 窄屏的目录：树整块收进一个原生下拉（optgroup 保留分组），
              比横向滚动的标签更省高度，也不用额外处理 30 篇文档的折行。 */}
          {!searching && (
            <select
              className="select docs-picker"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              aria-label="选择文档"
            >
              {groups.map((g) => (
                <optgroup key={g.category} label={g.label}>
                  {g.items.map((d) => (
                    <option key={d.path} value={d.path}>
                      {docTitle(d)}
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
          )}

          <div className={`docs-shell ${searching ? 'searching' : ''}`}>
            <div className="docs-nav">
              {searching ? (
                <SearchResults
                  results={results}
                  busy={searchBusy}
                  err={searchErr}
                  query={term}
                  active={path}
                  onPick={setPath}
                />
              ) : (
                <nav className="docs-tree" aria-label="文档目录">
                  {groups.map((g) => (
                    <div className="docs-group" key={g.category}>
                      <div className="docs-group-title">
                        {g.label}
                        <span className="docs-group-count">{g.items.length}</span>
                      </div>
                      {g.items.map((d) => (
                        <button
                          key={d.path}
                          type="button"
                          className={`docs-link ${d.path === path ? 'on' : ''}`}
                          aria-current={d.path === path ? 'page' : undefined}
                          onClick={() => setPath(d.path)}
                          title={d.path}
                        >
                          <span className="docs-link-title">{docTitle(d)}</span>
                          {d.generated && <span className="docs-link-live">实时</span>}
                        </button>
                      ))}
                    </div>
                  ))}
                </nav>
              )}
            </div>

            <section className="docs-doc">
              {!path ? (
                <div className="docs-empty">从左侧选一篇文档</div>
              ) : docLoading ? (
                <>
                  <div className="docs-doc-head">
                    <div className="skeleton" style={{ height: 22, width: '52%' }} />
                    <div className="skeleton" style={{ height: 12, width: '34%' }} />
                  </div>
                  <Skel rows={8} />
                </>
              ) : docErr ? (
                <Notice kind="error">
                  读取文档失败：{docErr}
                  <div className="notice-sub">
                    <span className="mono small">{path}</span>
                    <button className="btn small" onClick={() => setDocReload((n) => n + 1)}>
                      重试
                    </button>
                  </div>
                </Notice>
              ) : doc ? (
                <>
                  <div className="docs-doc-head">
                    <div className="docs-doc-title">{docTitle(doc)}</div>
                    <div className="docs-meta">
                      <span className="dot-tag dim">{categoryLabel(doc.category)}</span>
                      {updated !== '—' && <span>更新于 {updated}</span>}
                      <span className="docs-path">{doc.path}</span>
                      {doc.generated && (
                        <span
                          className="docs-badge"
                          title="不是 docs/ 里的文件：内容由能力注册表与 /v1/meta 在请求时生成，不会和代码漂移。"
                        >
                          实时生成
                        </span>
                      )}
                    </div>
                  </div>
                  {bodyHtml ? (
                    // renderMarkdown 的输出已整体转义，标签只可能是白名单里的那些。
                    <div
                      className="md docs-body"
                      dangerouslySetInnerHTML={{ __html: bodyHtml }}
                    />
                  ) : (
                    <div className="docs-empty">这篇文档没有正文。</div>
                  )}
                </>
              ) : null}
            </section>
          </div>
        </>
      )}
    </div>
  )
}

/** 未启用：只回答「没开」和「怎么开」，不画空目录。 */
function DocsDisabled({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="card">
      <div className="card-head">文档模块未启用</div>
      <div className="card-body">
        <Notice kind="warn">
          GET /v1/docs 返回 503：服务端没有装配文档库（文档根目录不存在或不可读），
          目录与正文都拿不到。
          <div className="notice-sub">
            <button className="btn small" onClick={onRetry}>
              重新检查
            </button>
          </div>
        </Notice>
        <div className="settings-kv">
          <div className="settings-kv-row">
            <div className="settings-kv-key mono">make build</div>
            <div className="settings-kv-val dim small">
              把仓库里的 <code>docs/</code> 拷进 <code>bin/docs</code>。
              文档不 embed 进二进制——那样改一篇文档也要重新编译。
            </div>
          </div>
          <div className="settings-kv-row">
            <div className="settings-kv-key mono">&lt;dataDir&gt;/docs</div>
            <div className="settings-kv-val dim small">
              控制面启动时把 <code>bin/docs</code> 同步到这里，文档模块只读这个目录。
              目录为空时按未启用处理。
            </div>
          </div>
          <div className="settings-kv-row">
            <div className="settings-kv-key mono">功能总览</div>
            <div className="settings-kv-val dim small">
              唯一不依赖 docs/ 的一篇：由能力注册表与 /v1/meta 在请求时生成。
              它也不可用时说明能力注册表没接上。
            </div>
          </div>
        </div>
        <div className="settings-notes">
          <p>
            文档模块只读，缺了它不影响平台其它功能。补上 docs/ 后重启控制面即可，
            不需要重新编译前端。
          </p>
        </div>
      </div>
    </div>
  )
}

function SearchResults({
  results,
  busy,
  err,
  query,
  active,
  onPick,
}: {
  results: DocMeta[] | null
  busy: boolean
  err: string
  query: string
  active: string
  onPick: (path: string) => void
}) {
  if (err) {
    return (
      <Notice kind="error">
        搜索失败：{err}
      </Notice>
    )
  }
  if (busy && results === null) return <Skel rows={6} />
  if (results === null) return null
  if (results.length === 0) {
    return <div className="docs-empty">没有命中「{query}」的文档</div>
  }
  return (
    <div className="docs-results">
      {results.map((d) => (
        <button
          key={d.path}
          type="button"
          className={`docs-result ${d.path === active ? 'on' : ''}`}
          onClick={() => onPick(d.path)}
          title={d.path}
        >
          <span className="docs-result-title">
            <Marked text={docTitle(d)} query={query} />
          </span>
          <span className="docs-result-path">
            {categoryLabel(d.category)} · {d.path}
          </span>
          {d.summary && (
            <span className="docs-result-sum">
              <Marked text={d.summary} query={query} />
            </span>
          )}
        </button>
      ))}
    </div>
  )
}

/** 命中片段高亮。切分在 docsModel 里，这里只把命中段包成 <mark>。 */
function Marked({ text, query }: { text: string; query: string }) {
  return (
    <>
      {highlightSegments(text, query).map((seg, i) =>
        seg.hit ? (
          <mark className="docs-hit" key={i}>
            {seg.text}
          </mark>
        ) : (
          <span key={i}>{seg.text}</span>
        ),
      )}
    </>
  )
}

function Skel({ rows }: { rows: number }) {
  return (
    <div className="sk-stack" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <div
          key={i}
          className="skeleton"
          style={{ height: 12, width: `${90 - (i % 3) * 16}%` }}
        />
      ))}
    </div>
  )
}
