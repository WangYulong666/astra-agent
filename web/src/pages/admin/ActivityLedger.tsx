// 用户视角行动账本（管理后台 tab）。
//
// 与审计日志的区别：这里按用户视角把「它做了什么、卡在哪」摊成一条时间线。
// 两个容易被做坏的判断写在最前面：
//
// 1. 默认（没输入 id）不画空图表，明确提示「输入 run id 查看」；
// 2. 能力未启用（503）时明确说「能力未启用」，而不是给一张看起来「在跑、
//    只是还没数据」的空表——后者会把运维引去排查 agent，而不是去开能力开关。

import { useState } from 'react'
import type { FormEvent } from 'react'
import { activityApi, ApiError } from '../../api'
import type { ActivityEntry, ActivitySummary } from '../../api'
import { fmtTime } from '../../format'
import { Empty, Loading, Notice } from '../../components/ui'
import {
  countStatuses,
  emptyHint,
  kindMeta,
  statusMeta,
} from './activityModel'

type Mode = 'run' | 'session'

type View = {
  entries: ActivityEntry[]
  summary: ActivitySummary
}

export function ActivityLedger() {
  const [mode, setMode] = useState<Mode>('run')
  const [input, setInput] = useState('')
  // applied 与输入分开：正在打的字不该触发一次查询。
  const [applied, setApplied] = useState<{ mode: Mode; id: string } | null>(null)
  const [view, setView] = useState<View | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [disabled, setDisabled] = useState(false)

  const load = async (m: Mode, id: string) => {
    setBusy(true)
    setErr('')
    setDisabled(false)
    setView(null)
    try {
      if (m === 'run') {
        const res = await activityApi.runActivity(id)
        setView({ entries: res.entries || [], summary: res.summary })
      } else {
        // session 视图后端只回 entries，计数在前端现算。
        const res = await activityApi.sessionActivity(id)
        const entries = res.entries || []
        setView({ entries, summary: countStatuses(entries) })
      }
    } catch (e) {
      if (e instanceof ApiError && e.status === 503) {
        setDisabled(true)
      } else {
        setErr((e as Error).message)
      }
    } finally {
      setBusy(false)
    }
  }

  const submit = (e: FormEvent) => {
    e.preventDefault()
    const id = input.trim()
    if (!id) return
    setApplied({ mode, id })
    void load(mode, id)
  }

  const switchMode = (m: Mode) => {
    if (m === mode) return
    setMode(m)
    setApplied(null)
    setView(null)
    setErr('')
    setDisabled(false)
    setInput('')
  }

  const refresh = () => {
    if (applied) void load(applied.mode, applied.id)
  }

  const s = view?.summary
  const counts = [
    { key: 'done', label: '已完成', tone: 'done', value: s?.done ?? 0 },
    { key: 'running', label: '进行中', tone: 'running', value: s?.running ?? 0 },
    { key: 'blocked', label: '阻塞', tone: 'blocked', value: s?.blocked ?? 0 },
    { key: 'failed', label: '失败', tone: 'failed', value: s?.failed ?? 0 },
  ]
  const showData = applied && !busy && !disabled && !err && view

  return (
    <div className="page ledger-page">
      <div className="page-head">
        <div>
          <div className="page-title">行动账本</div>
          <div className="page-sub">
            把事件流、审批、出网与子任务聚成一条按时间排序、带状态的时间线，
            一眼看出「它做了什么、现在卡在哪」。输入 run id 或 session id 查看。
          </div>
        </div>
      </div>

      <div className="card">
        <div className="card-head">查询</div>
        <div className="card-body">
          <form className="ledger-filter" onSubmit={submit}>
            <div className="seg" role="group" aria-label="查询维度">
              <button
                type="button"
                className={`seg-item ${mode === 'run' ? 'on' : ''}`}
                onClick={() => switchMode('run')}
              >
                按 run
              </button>
              <button
                type="button"
                className={`seg-item ${mode === 'session' ? 'on' : ''}`}
                onClick={() => switchMode('session')}
              >
                按 session
              </button>
            </div>
            <input
              className="input ledger-input"
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder={mode === 'run' ? '输入 run id' : '输入 session id'}
              spellCheck={false}
              aria-label={mode === 'run' ? 'run id' : 'session id'}
            />
            <button className="btn" type="submit" disabled={busy || !input.trim()}>
              查看
            </button>
            {applied && (
              <button className="btn" type="button" disabled={busy} onClick={refresh}>
                {busy ? '刷新中…' : '刷新'}
              </button>
            )}
          </form>
        </div>
      </div>

      {/* 默认空态：没查过就别画空图表 */}
      {!applied && !busy && <Empty text={emptyHint('no-id', mode)} />}

      {applied && busy && <Loading />}

      {/* 未启用（503）：明确说「能力未启用」，不给空图表 */}
      {applied && !busy && disabled && (
        <div className="ledger-disabled">
          <Notice kind="warn">
            {emptyHint('disabled', mode)}
            <div className="notice-sub">
              服务端返回 503，说明行动账本能力当前未启用。请检查 <code>/v1/meta</code> 的{' '}
              <code>activity</code> 开关与装配日志。
            </div>
          </Notice>
        </div>
      )}

      {applied && !busy && err && (
        <Notice kind="error">
          {err}
          <div className="notice-sub">
            <button className="btn small" onClick={refresh}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {showData && (
        <>
          <div className="ledger-stats">
            {counts.map((c) => (
              <div className="stat ledger-stat" key={c.key}>
                <div className="stat-k">{c.label}</div>
                <div className={`stat-v ledger-v-${c.tone}`}>{c.value}</div>
              </div>
            ))}
          </div>

          {s?.current && (
            <Notice kind="warn">
              <span className="ledger-current">
                现在卡在：<strong>{s.current}</strong>
              </span>
            </Notice>
          )}

          <div className="card">
            <div className="card-head">
              时间线
              <span className="dim small"> · 共 {view.entries.length} 条（按时间由早到晚）</span>
            </div>
            {view.entries.length === 0 ? (
              <Empty text={emptyHint('no-entries', mode)} />
            ) : (
              <div className="ledger-timeline">
                {view.entries.map((e, i) => (
                  <TimelineRow key={`${e.time}-${i}`} entry={e} />
                ))}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}

function TimelineRow({ entry }: { entry: ActivityEntry }) {
  const km = kindMeta(entry.kind)
  const sm = statusMeta(entry.status)
  const ref = entry.ref || entry.runId
  return (
    <div className="ledger-row">
      <div className="ledger-rail" aria-hidden="true">
        <span className={`ledger-dot ledger-tone-${km.tone}`}>{km.glyph}</span>
      </div>
      <div className="ledger-item">
        <div className="ledger-item-head">
          <span className="ledger-time">{fmtTime(entry.time)}</span>
          <span className={`ledger-kind ledger-tone-${km.tone}`}>{km.label}</span>
          <span className={`ledger-status ledger-tone-${sm.tone}`}>{sm.label}</span>
        </div>
        <div className="ledger-title">{entry.title}</div>
        {entry.detail && <div className="ledger-detail">{entry.detail}</div>}
        {ref && <div className="ledger-ref mono small dim">ref: {ref}</div>}
      </div>
    </div>
  )
}
