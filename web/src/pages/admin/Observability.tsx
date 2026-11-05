// 可观测（Langfuse）。
//
// 这一页有两个容易被做坏的判断，写在最前面：
//
// 1. **没开观测时不要画空图表。** 一张空表格会被读成「观测在跑、只是还没数据」，
//    运维接着就去查 agent 的毛病了。未启用时这一屏只回答两件事：现在没开、
//    开它要配哪几个变量。
//
// 2. **trace 详情不在站内复刻。** 服务端拿 secret key 代查，浏览器只有 public
//    key，所以点行 = 新窗口打开 Langfuse 自己的页面。在这里画一个「简化版
//    瀑布图」既不完整又会和上游不一致。

import { useCallback, useEffect, useState } from 'react'
import { obsApi } from '../../api'
import type { ObsConfig, ObsTrace } from '../../api'
import { clockTime, fmtTime, shortId } from '../../format'
import { Empty, Notice } from '../../components/ui'

/** 自动刷新周期。30s 是「盯发布」的节奏：比手动刷快，又不至于把 Langfuse 打满。 */
const REFRESH_MS = 30_000
const LIMIT = 20

export function Observability() {
  const [cfg, setCfg] = useState<ObsConfig | null>(null)
  const [err, setErr] = useState('')

  const loadCfg = useCallback(async () => {
    setErr('')
    setCfg(null)
    try {
      setCfg(await obsApi.config())
    } catch (e) {
      setErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void loadCfg()
  }, [loadCfg])

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">可观测</div>
          <div className="page-sub">
            agent 每轮对话的 trace 发到 Langfuse。控制面代查（secret key 只留在服务端），
            这一页只用来确认「开着没」和「最近跑了什么」。
          </div>
        </div>
        {cfg?.enabled && cfg.dashboard_url && (
          <a className="btn page-act" href={cfg.dashboard_url} target="_blank" rel="noreferrer">
            打开 Langfuse
          </a>
        )}
      </div>

      {err && (
        <Notice kind="error">
          {err}
          <div className="notice-sub">
            <button className="btn small" onClick={() => void loadCfg()}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {!cfg && !err && (
        <div className="card">
          <div className="card-head">观测状态</div>
          <Skel rows={3} />
        </div>
      )}

      {cfg && <StatusCard cfg={cfg} onRecheck={() => void loadCfg()} />}

      {/* 关着的时候：给配置清单，不给表格。反过来，开着的时候也不重复贴变量名。 */}
      {cfg && !cfg.enabled && <DisabledGuide />}

      {cfg?.enabled && <TracesCard />}
    </div>
  )
}

function StatusCard({ cfg, onRecheck }: { cfg: ObsConfig; onRecheck: () => void }) {
  return (
    <div className="card">
      <div className="card-head">观测状态</div>
      <div className="card-body">
        <div className="settings-gateway-state">
          <span className={`dot-tag ${cfg.enabled ? 'ok' : 'off'}`}>
            {cfg.enabled ? '已启用' : '未启用'}
          </span>
          {cfg.enabled ? (
            <span className="dim small">trace 正在上报，下面的表格是 Langfuse 里的最近 {LIMIT} 条</span>
          ) : (
            <span className="dim small">没有配置 Langfuse，trace 不会产生，也不会有任何数据被发出去</span>
          )}
        </div>

        {cfg.enabled ? (
          <div className="settings-kv">
            <Kv k="上报端点" v={<span className="mono small">{cfg.endpoint || '—'}</span>} />
            <Kv
              k="public key"
              v={<span className="mono small">{cfg.public_key || '—'}</span>}
            />
            <Kv
              k="面板地址"
              v={
                cfg.dashboard_url ? (
                  <a className="mono small" href={cfg.dashboard_url} target="_blank" rel="noreferrer">
                    {cfg.dashboard_url}
                  </a>
                ) : (
                  '—'
                )
              }
            />
          </div>
        ) : (
          <div className="settings-actions">
            <button className="btn" onClick={onRecheck}>
              重新检查
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

/** 未启用时的开箱指引。三个变量缺一不可，所以逐条列出来而不是写一句「去配 Langfuse」。 */
function DisabledGuide() {
  return (
    <div className="card">
      <div className="card-head">怎么打开</div>
      <div className="card-body">
        <div className="settings-kv">
          <Kv
            k="LANGFUSE_HOST"
            v={
              <span className="dim small">
                Langfuse 服务地址，如 <code>http://127.0.0.1:3000</code>。留空 = 关闭观测。
              </span>
            }
          />
          <Kv
            k="LANGFUSE_PUBLIC_KEY"
            v={<span className="dim small">项目公钥（pk-lf-…），可以给浏览器看，会出现在上面的状态卡里。</span>}
          />
          <Kv
            k="LANGFUSE_SECRET_KEY"
            v={<span className="dim small">项目私钥（sk-lf-…），只留在服务端，不会下发到前端。</span>}
          />
        </div>
        <div className="settings-notes">
          <p>
            三项都配上后重启控制面。缺任意一项都会按「未启用」处理——平台照常可用，
            只是不产生 trace。本地起一套 Langfuse 用 <code>make obs-up</code>，状态用 <code>make deps-status</code> 看。
          </p>
        </div>
      </div>
    </div>
  )
}

function TracesCard() {
  const [runId, setRunId] = useState('')
  // applied 与 runId 分开：输入框里正在打的字不该影响 30s 自动刷新查的是什么。
  const [applied, setApplied] = useState('')
  const [items, setItems] = useState<ObsTrace[] | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [auto, setAuto] = useState(false)
  const [at, setAt] = useState('')

  const load = useCallback(async (rid: string, quiet = false) => {
    if (!quiet) {
      setBusy(true)
      setItems(null)
    }
    try {
      const res = await obsApi.traces(rid, LIMIT)
      setItems(res.items)
      setErr('')
      setAt(new Date().toISOString())
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      if (!quiet) setBusy(false)
    }
  }, [])

  useEffect(() => {
    void load('')
  }, [load])

  useEffect(() => {
    if (!auto) return
    const id = window.setInterval(() => void load(applied, true), REFRESH_MS)
    return () => window.clearInterval(id)
  }, [auto, applied, load])

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const next = runId.trim()
    setApplied(next)
    void load(next)
  }

  return (
    <>
      <div className="card">
        <div className="card-head">最近 trace</div>

        <div className="card-body">
          <form className="filter-bar" onSubmit={submit}>
            <input
              className="input"
              value={runId}
              onChange={(e) => setRunId(e.target.value)}
              placeholder="按 run_id 过滤，留空查全部"
              spellCheck={false}
            />
            <button className="btn" type="submit" disabled={busy}>
              查询
            </button>
            <button className="btn" type="button" disabled={busy} onClick={() => void load(applied)}>
              {busy ? '刷新中…' : '刷新'}
            </button>
            <label className="check">
              <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
              自动刷新（30s）
            </label>
            {at && <span className="dim small">上次更新 {clockTime(at)}</span>}
          </form>

          {err && (
            <Notice kind="error">
              {err}
              <div className="notice-sub">
                <button className="btn small" onClick={() => void load(applied)}>
                  重试
                </button>
              </div>
            </Notice>
          )}
        </div>

        {items === null && !err ? (
          <Skel rows={5} />
        ) : items && items.length === 0 && !err ? (
          <Empty
            text={
              applied
                ? `没有 run_id 匹配「${applied}」的 trace`
                : 'Langfuse 里还没有 trace：跑一条指令之后再回来看'
            }
          />
        ) : items && items.length > 0 ? (
          <div className="tbl-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th style={{ width: 130 }}>时间</th>
                  <th style={{ width: 130 }}>run_id</th>
                  <th>name</th>
                  <th style={{ width: 90 }}>状态</th>
                  <th className="num" style={{ width: 80 }}>耗时</th>
                  <th>输入摘要</th>
                </tr>
              </thead>
              <tbody>
                {items.map((t) => (
                  <tr
                    key={t.id}
                    className={t.url ? 'link' : ''}
                    title={t.url ? '在新窗口打开 Langfuse trace' : '这条 trace 没有可打开的地址'}
                    onClick={() => {
                      if (t.url) window.open(t.url, '_blank', 'noopener,noreferrer')
                    }}
                  >
                    <td className="dim">{fmtTime(t.started_at)}</td>
                    <td className="mono small">{t.run_id ? shortId(t.run_id) : '—'}</td>
                    <td>
                      <span className="cell-cut" title={t.name || ''}>
                        {t.name || '—'}
                      </span>
                    </td>
                    <td>
                      <StatusTag status={t.status} />
                    </td>
                    <td className="num">{fmtMs(t.latency_ms)}</td>
                    <td>
                      <span className="cell-cut" title={t.input || ''}>
                        {t.input || <span className="dim">（无摘要）</span>}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </div>
    </>
  )
}

function StatusTag({ status }: { status?: string }) {
  const s = (status || '').toLowerCase()
  // Langfuse 的 level 只有 success / error / warning 几种；未知值走「中性」，
  // 不用红色——把未知状态标红会让一次误报变成一场排障。
  const cls = s === 'success' || s === 'completed' ? 'ok' : s === 'error' || s === 'failed' ? 'off' : 'dim'
  return <span className={`dot-tag ${cls}`}>{status || '—'}</span>
}

/** 耗时：毫秒级看 ms，超过 1s 换秒（1200ms 要读成 1.2s 才有量级感）。 */
function fmtMs(ms?: number): string {
  if (!ms || ms < 0) return '—'
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

function Kv({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="settings-kv-row">
      <div className="settings-kv-key">{k}</div>
      <div className="settings-kv-val">{v}</div>
    </div>
  )
}

/** 加载骨架：比转圈更能说明「这里将出现几行表格」，也不会在慢网下反复跳高度。 */
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
