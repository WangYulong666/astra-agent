// 审计日志。
//
// 这一页的读者通常是「事后查一件事」：谁在什么时候动了某个对象。
// 所以默认按时间倒序、给足列宽、ID 保留可读前缀——不做折叠，也不做分页
// （后端有 limit，运维翻几百条是常态）。

import { useCallback, useEffect, useRef, useState } from 'react'
import { adminApi } from '../../api'
import { fmtTime, shortId } from '../../format'
import type { AuditLog } from '../../types'
import { Empty, Loading, Notice } from '../../components/ui'

type Span = '1d' | '7d' | '30d' | 'all'

const SPAN_LABEL: Record<Span, string> = {
  '1d': '近 24 小时',
  '7d': '近 7 天',
  '30d': '近 30 天',
  all: '全部',
}

const COMMON_ACTIONS = [
  'auth.login',
  'auth.logout',
  'auth.password.change',
  'user.create',
  'user.update',
  'apikey.create',
  'apikey.revoke',
  'tenant.update',
  'sandbox.destroy',
  'session.create',
]

function spanStart(s: Span): string {
  if (s === 'all') return ''
  const d = new Date()
  d.setDate(d.getDate() - (s === '1d' ? 1 : s === '7d' ? 7 : 30))
  return d.toISOString()
}

export function Audit() {
  const [action, setAction] = useState('')
  const [span, setSpan] = useState<Span>('7d')
  const [rows, setRows] = useState<AuditLog[] | null>(null)
  const [err, setErr] = useState('')

  const load = useCallback(async (a: string, s: Span) => {
    setErr('')
    try {
      setRows(
        await adminApi.audit({
          action: a.trim(),
          from: spanStart(s),
          limit: 200,
        }),
      )
    } catch (e) {
      setErr((e as Error).message)
      setRows([])
    }
  }, [])

  // 时间范围变了要自动重查；但 action 是用户敲完才提交的，不能进依赖数组，
  // 否则每敲一个字都发一次请求。用 ref 拿到「当前过滤词」，避免闭包读到旧值。
  const actionRef = useRef(action)
  actionRef.current = action
  useEffect(() => {
    void load(actionRef.current, span)
  }, [load, span])

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">审计</div>
          <div className="page-sub">
            登录、改权限、发凭据、强杀沙箱都会落一条。只读，不可修改或删除。
          </div>
        </div>
        <div className="seg">
          {(Object.keys(SPAN_LABEL) as Span[]).map((s) => (
            <button
              key={s}
              className={`seg-item ${span === s ? 'on' : ''}`}
              onClick={() => setSpan(s)}
            >
              {SPAN_LABEL[s]}
            </button>
          ))}
        </div>
      </div>

      <form
        className="filter-bar"
        onSubmit={(e) => {
          e.preventDefault()
          void load(action, span)
        }}
      >
        <input
          className="input"
          list="audit-actions"
          value={action}
          onChange={(e) => setAction(e.target.value)}
          placeholder="按动作过滤，如 user.update（留空看全部）"
        />
        <datalist id="audit-actions">
          {COMMON_ACTIONS.map((a) => (
            <option key={a} value={a} />
          ))}
        </datalist>
        <button className="btn" type="submit">
          查询
        </button>
        {action && (
          <button
            className="link-btn"
            type="button"
            onClick={() => {
              setAction('')
              void load('', span)
            }}
          >
            清除过滤
          </button>
        )}
      </form>

      {err && <Notice kind="error">{err}</Notice>}

      <div className="tbl-wrap">
        {rows === null ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty text="没有匹配的审计记录" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th style={{ width: 160 }}>时间</th>
                <th style={{ width: 210 }}>操作者</th>
                <th style={{ width: 170 }}>动作</th>
                <th>目标</th>
                <th style={{ width: 130 }}>IP</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((l) => (
                <tr key={l.id}>
                  <td className="dim">{fmtTime(l.createdAt)}</td>
                  <td>
                    {l.actorEmail ? (
                      <span className="mono small">{l.actorEmail}</span>
                    ) : (
                      <span className="dim mono small">{l.userId ? shortId(l.userId) : '系统'}</span>
                    )}
                  </td>
                  <td>
                    <span className="dot-tag">{l.action}</span>
                  </td>
                  <td className="dim" title={l.detail ? JSON.stringify(l.detail) : undefined}>
                    {l.targetType}
                    {l.targetId ? ` · ${shortId(l.targetId)}` : ''}
                  </td>
                  <td className="dim mono small">{l.ip || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
