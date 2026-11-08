// 失败诊断（/admin/insights）。
//
// 数据来源 GET /v1/insights：internal/insight 订阅 run 终态，对 failed/cancelled
// 生成结构化诊断（error / lastTool / suggestion …）。两个判断写在最前面：
//
// 1. 默认不填 session_id = 全租户最近诊断；填了才按 session_id 收窄。
// 2. 空数据时明确说「还没有失败诊断」，而不是一张空白表；503 未启用给引导，
//    不画一张会被读成「诊断在跑、只是还没数据」的空图表。

import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { insightApi } from '../../api'
import type { Insight } from '../../api'
import { Empty, Notice } from '../../components/ui'
import { fmtTime, shortId } from '../../format'
import { emptyText, insightStatus, isUnavailable, truncate } from './loopModel'
import { DisabledGuide, Skel } from './LoopGuide'

const LIMIT = 20

export function Insights() {
  const [input, setInput] = useState('')
  const [applied, setApplied] = useState('')
  const [items, setItems] = useState<Insight[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [disabled, setDisabled] = useState(false)

  const load = useCallback(async (sessionId: string, quiet = false) => {
    if (!quiet) {
      setBusy(true)
      setItems(null)
    }
    setErr('')
    setDisabled(false)
    try {
      const res = await insightApi.list({ sessionId: sessionId || undefined, limit: LIMIT })
      setItems(res.items || [])
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setErr((e as Error).message)
    } finally {
      if (!quiet) setBusy(false)
    }
  }, [])

  useEffect(() => {
    // 默认全租户：不带 session_id 查一次。
    void load('')
  }, [load])

  const submit = (e: FormEvent) => {
    e.preventDefault()
    const next = input.trim()
    setApplied(next)
    void load(next)
  }

  const refresh = () => void load(applied)

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">失败诊断</div>
          <div className="page-sub">
            失败或取消的 run 会生成一条结构化诊断：错误、最后调用的工具、规则式建议。
            按 session_id 可收窄到某个会话，留空看全租户最近 {LIMIT} 条。
          </div>
        </div>
        <button className="btn page-act" onClick={refresh} disabled={busy}>
          刷新
        </button>
      </div>

      <div className="notice info loop-note">
        诊断会作为下一轮 run 的上下文前缀回灌（<code>INSIGHT_REPLAY</code> 可关）；它是只读建议，
        不会自动改平台代码或自动重试。
      </div>

      {/* 未启用时不渲染一个必然 503 的输入框（同 Memory 页的取舍）。 */}
      {!disabled && (
        <div className="card">
          <div className="card-head">过滤</div>
          <div className="card-body">
            <form className="filter-bar" onSubmit={submit}>
              <input
                className="input"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                placeholder="按 session_id 过滤，留空查全部"
                spellCheck={false}
                aria-label="session_id"
              />
              <button className="btn" type="submit" disabled={busy}>
                查询
              </button>
              {applied && (
                <span className="dim small">当前 session：{shortId(applied)}</span>
              )}
            </form>
          </div>
        </div>
      )}

      {err && (
        <Notice kind="error">
          读取失败诊断失败（{err}）
          <div className="notice-sub">
            <button className="btn small" onClick={refresh}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {disabled ? (
        <DisabledGuide page="insights" onRetry={refresh} />
      ) : !err ? (
        <div className="card">
          <div className="card-head">
            最近诊断
            {items && <span className="dim small"> · 共 {items.length} 条</span>}
          </div>

          {items === null && busy ? (
            <Skel rows={5} />
          ) : items && items.length === 0 ? (
            <Empty text={emptyText('insights', applied !== '')} />
          ) : items && items.length > 0 ? (
            <div className="tbl-wrap loop-table">
              <table className="tbl">
                <thead>
                  <tr>
                    <th style={{ width: 150 }}>时间</th>
                    <th style={{ width: 120 }}>RUN</th>
                    <th style={{ width: 84 }}>状态</th>
                    <th>错误</th>
                    <th style={{ width: 130 }}>最后工具</th>
                    <th>建议</th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((it, i) => (
                    <tr key={`${it.runId}-${it.createdAt}-${i}`}>
                      <td className="dim">{fmtTime(it.createdAt)}</td>
                      <td className="mono small" title={it.runId}>
                        {shortId(it.runId)}
                      </td>
                      <td>
                        <span className={`loop-tag loop-tag-${insightStatus(it.status).tone}`}>
                          {insightStatus(it.status).label}
                        </span>
                      </td>
                      <td>
                        <span className="loop-cell-cut" title={it.error || ''}>
                          {truncate(it.error, 48) || <span className="dim">—</span>}
                        </span>
                      </td>
                      <td className="loop-cell-mono" title={it.lastTool || ''}>
                        <span className="loop-cell-cut">{it.lastTool || '—'}</span>
                      </td>
                      <td>
                        <span className="loop-cell-cut" title={it.suggestion || ''}>
                          {truncate(it.suggestion, 48) || <span className="dim">—</span>}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}
