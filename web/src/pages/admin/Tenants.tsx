// 租户管理。只有 owner 能进（admin 的权限表里没有 tenant:manage）。
//
// 这一页的风险全在「停用」上：租户一停，它下面的所有用户立刻登不进来，
// 包括你自己；而 owner 只能看见自己的租户，所以停了就没人能在界面上恢复。
// 确认框必须把这句说清楚，否则迟早有人点着玩。

import { useCallback, useEffect, useState } from 'react'
import { adminApi } from '../../api'
import { useAuth } from '../../auth'
import { fmtNumber, fmtTime } from '../../format'
import { STATUS_TEXT } from '../../types'
import type { Tenant } from '../../types'
import { Empty, Loading, Notice } from '../../components/ui'

export function Tenants() {
  const { tenant: mine } = useAuth()
  const [rows, setRows] = useState<Tenant[] | null>(null)
  const [err, setErr] = useState('')
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState('')

  const load = useCallback(async () => {
    setErr('')
    try {
      setRows(await adminApi.listTenants())
    } catch (e) {
      setErr((e as Error).message)
      setRows([])
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const patch = async (id: string, body: { status?: string; monthlyTokenLimit?: number }) => {
    setErr('')
    setBusy(id)
    try {
      const next = await adminApi.updateTenant(id, body)
      setRows((prev) => prev?.map((t) => (t.id === id ? next : t)) ?? null)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const saveLimit = (t: Tenant) => {
    const raw = drafts[t.id]
    if (raw === undefined) return
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0) {
      setErr('月度额度必须是不小于 0 的整数（0 表示不限）')
      return
    }
    void patch(t.id, { monthlyTokenLimit: Math.floor(n) })
  }

  const toggleStatus = (t: Tenant) => {
    const suspending = t.status === 'active'
    if (suspending) {
      const ok = window.confirm(
        `停用租户「${t.name}」（${t.slug}）？\n\n` +
          `停用后该租户下所有账号立刻无法登录，进行中的请求也会开始 401。` +
          `owner 只能看到自己的租户，所以这一步在界面上没有撤销入口，` +
          `只能由运维在数据库里改回 active。`,
      )
      if (!ok) return
    }
    void patch(t.id, { status: suspending ? 'suspended' : 'active' })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">租户</div>
          <div className="page-sub">
            月度额度是「最多用到这个数」，用满即拒绝新的调用；0 表示不限。
          </div>
        </div>
      </div>

      {err && <Notice kind="error">{err}</Notice>}

      <div className="tbl-wrap">
        {rows === null ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty text="没有可见的租户" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th>名称</th>
                <th style={{ width: 150 }}>slug</th>
                <th style={{ width: 90 }}>状态</th>
                <th style={{ width: 230 }}>月度 token 额度</th>
                <th style={{ width: 160 }}>创建时间</th>
                <th style={{ width: 90 }} />
              </tr>
            </thead>
            <tbody>
              {rows.map((t) => {
                const draft = drafts[t.id] ?? String(t.monthlyTokenLimit)
                const self = mine?.id === t.id
                return (
                  <tr key={t.id} className={t.status !== 'active' ? 'off' : ''}>
                    <td>
                      {t.name}
                      {self && <span className="tag">当前</span>}
                    </td>
                    <td>
                      <span className="mono small">{t.slug}</span>
                    </td>
                    <td>
                      <span className={`dot-tag ${t.status === 'active' ? 'ok' : 'off'}`}>
                        {STATUS_TEXT[t.status] || t.status}
                      </span>
                    </td>
                    <td>
                      <div className="inline-edit">
                        <input
                          className="input slim"
                          type="number"
                          min={0}
                          value={draft}
                          onChange={(e) =>
                            setDrafts((prev) => ({ ...prev, [t.id]: e.target.value }))
                          }
                          title={t.monthlyTokenLimit === 0 ? '当前不限' : fmtNumber(t.monthlyTokenLimit)}
                        />
                        <button
                          className="btn small"
                          disabled={busy === t.id || draft === String(t.monthlyTokenLimit)}
                          onClick={() => saveLimit(t)}
                        >
                          保存
                        </button>
                      </div>
                      <div className="hint-line">
                        {t.monthlyTokenLimit === 0 ? '不限' : `当前 ${fmtNumber(t.monthlyTokenLimit)}`}
                      </div>
                    </td>
                    <td className="dim">{fmtTime(t.createdAt)}</td>
                    <td>
                      <div className="row-actions">
                        <button
                          className={`btn small ${t.status === 'active' ? 'danger' : ''}`}
                          disabled={busy === t.id}
                          onClick={() => toggleStatus(t)}
                        >
                          {t.status === 'active' ? '停用' : '启用'}
                        </button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
