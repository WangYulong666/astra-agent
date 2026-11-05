// 向量记忆（Milvus）。
//
// 这一页很容易被做成「全租户记忆管理」，那是个错误的产品判断：
//
//   GET /v1/memory/search 是**当前登录用户自己的**记忆——服务端强制用调用者的
//   tenant_id + user_id 拼过滤表达式，前端没有、也不该有「查别人」的参数。
//
// 所以页面主线是「我自己的记忆长什么样」，运营视角只额外给一个**只读统计**区块，
// 用一句话说清隔离是怎么做到的，不提供任何跨用户查询入口。
//
// 未启用时（没配 MILVUS_ADDR）不渲染搜索框：一个必然 503 的输入框，
// 比直接告诉用户「还差一个环境变量」更浪费时间。

import { useCallback, useEffect, useState } from 'react'
import { memoryApi } from '../../api'
import type { MemoryItem, MemoryStats } from '../../api'
import { fmtNumber, fmtTime } from '../../format'
import { Empty, Notice } from '../../components/ui'

const K_OPTIONS = [5, 10, 20]

export function Memory() {
  const [stats, setStats] = useState<MemoryStats | null>(null)
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    setErr('')
    setStats(null)
    try {
      setStats(await memoryApi.stats())
    } catch (e) {
      setErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">记忆</div>
          <div className="page-sub">
            长期记忆存在 Milvus 里，供 agent 回答「上次那个方案」这类跨会话问题。
            本页的检索结果只包含<strong>当前登录用户自己的记忆</strong>，不含别人的。
          </div>
        </div>
        <button className="btn page-act" onClick={() => void load()}>
          刷新
        </button>
      </div>

      {err && (
        <Notice kind="error">
          {err}
          <div className="notice-sub">
            <button className="btn small" onClick={() => void load()}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {!stats && !err && (
        <div className="card">
          <div className="card-head">存储状态</div>
          <Skel rows={3} />
        </div>
      )}

      {stats && (
        <>
          <div className="stats three">
            <Stat k="状态" v={stats.enabled ? '已启用' : '未启用'} />
            <Stat k="后端" v={stats.backend || '—'} />
            <Stat k="集合" v={stats.collection || '—'} />
            <Stat k="向量条数" v={stats.enabled ? fmtNumber(stats.count) : '—'} />
            <Stat k="维度" v={stats.dim > 0 ? String(stats.dim) : '—'} />
            <Stat k="Embedder" v={stats.embedder || '—'} />
          </div>

          {stats.enabled ? (
            <SearchPanel />
          ) : (
            <Notice kind="warn">
              记忆未启用：没有配置 <span className="mono">MILVUS_ADDR</span>。配置它（可选
              <span className="mono"> MEMORY_EMBED_BASE_URL / MEMORY_EMBED_API_KEY</span> 指定真正的
              embedding 端点）后重启控制面即可。平台其余功能不受影响。
            </Notice>
          )}

          <IsolationNote />
        </>
      )}
    </div>
  )
}

function SearchPanel() {
  const [q, setQ] = useState('')
  const [k, setK] = useState(10)
  // items === null 表示「还没查过」，与「查了但没命中」必须分开：
  // 前者该提示怎么用，后者该说没找到。
  const [items, setItems] = useState<MemoryItem[] | null>(null)
  const [searched, setSearched] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const text = q.trim()
    if (!text) return
    setBusy(true)
    setErr('')
    try {
      const res = await memoryApi.search(text, k)
      setItems(res.items)
      setSearched(text)
    } catch (e2) {
      setErr((e2 as Error).message)
      setItems(null)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">检索我的记忆</div>
      <div className="card-body">
        <form className="filter-bar" onSubmit={(e) => void submit(e)}>
          <input
            className="input"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="例如：上次定的接口分页方案"
            spellCheck={false}
          />
          <select className="select" value={k} onChange={(e) => setK(Number(e.target.value))}>
            {K_OPTIONS.map((n) => (
              <option key={n} value={n}>
                取 {n} 条
              </option>
            ))}
          </select>
          <button className="btn" type="submit" disabled={busy || !q.trim()}>
            {busy ? '检索中…' : '搜索'}
          </button>
        </form>

        {err && (
          <Notice kind="error">
            {err}
            <div className="notice-sub">检索失败时不会退回「全量列出」——那会把别人的记忆混进来。</div>
          </Notice>
        )}

        {!err && items === null && (
          <div className="hint-line">
            输入一句话检索。命中的是按语义相似度排的，score 越小越接近（L2 距离）。
          </div>
        )}
      </div>

      {busy ? (
        <Skel rows={2} />
      ) : items === null ? null : items.length === 0 ? (
        <Empty text={`没有命中「${searched}」的记忆：要么还没写入过，要么这条确实没被记下来`} />
      ) : (
        <div>
          {items.map((it) => (
            <Hit key={it.id} item={it} />
          ))}
        </div>
      )}
    </div>
  )
}

function Hit({ item }: { item: MemoryItem }) {
  const [open, setOpen] = useState(false)
  const long = item.text.length > 160

  return (
    <div className="mem-hit">
      <div className="mem-hit-head">
        {/* 三位小数：score 全在 0–1 附近，两位看不出排序，四位是噪声 */}
        <span className="mem-score">
          {typeof item.score === 'number' && item.score > 0 ? item.score.toFixed(3) : '—'}
        </span>
        <span className={`dot-tag ${item.kind === 'fact' ? 'ok' : 'dim'}`}>{item.kind}</span>
        <span className="dim small">{fmtTime(item.created_at)}</span>
        {item.session_id && (
          <span className="dim small mono">session {item.session_id.slice(0, 10)}…</span>
        )}
      </div>
      <div className={`mem-hit-text ${long && !open ? 'clamp' : ''}`}>{item.text}</div>
      {long && (
        <div className="mem-hit-more">
          <button className="btn small" onClick={() => setOpen((v) => !v)}>
            {open ? '收起' : '展开全文'}
          </button>
        </div>
      )}
    </div>
  )
}

/**
 * 运营视角的只读统计。
 *
 * 只解释隔离机制，不给任何「按租户看内容」的入口——那件事在契约里就是禁止的
 * （admin 也只能按显式指定的 user 查，不能全库扫）。把边界写在页面上，
 * 比藏在设计文档里有用：拿到 admin 账号的人第一反应就是想点进去看看。
 */
function IsolationNote() {
  return (
    <div className="card">
      <div className="card-head">只读统计（运营视角）</div>
      <div className="card-body settings-notes">
        <p>
          <strong>内容隔离：</strong>
          向量集合把 <code>tenant_id</code> 设为 partition key，每次检索/删除都再叠一条
          <code> tenant_id == … and user_id == …</code> 的过滤表达式；两个条件都取自调用者身份，
          请求体里传什么都不认。所以跨租户、跨用户在物理上就查不到，不是靠前端不显示。
        </p>
        <p>
          <strong>这里为什么不给「按用户检索」：</strong>
          那等于把全租户的记忆内容摊在一个页面上。需要做合规删除时走
          <code> DELETE /v1/memory/{"{id}"}</code>（同样带 tenant + user 过滤），
          或者用 <code>admin-cli memory stats --tenant</code> 只取条数、不取内容。
        </p>
      </div>
    </div>
  )
}

function Stat({ k, v }: { k: string; v: string }) {
  return (
    <div className="stat">
      <div className="stat-k">{k}</div>
      <div className="stat-v">{v}</div>
    </div>
  )
}

function Skel({ rows }: { rows: number }) {
  return (
    <div className="sk-stack" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <div
          key={i}
          className="skeleton"
          style={{ height: 12, width: `${92 - (i % 3) * 14}%` }}
        />
      ))}
    </div>
  )
}
