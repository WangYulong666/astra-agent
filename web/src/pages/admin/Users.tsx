// 用户管理。
//
// 这一页唯一容易出错的地方是「最后一个 owner」：后端会回 409 拒绝降级/停用，
// 前端要做的是把这句话原样、显眼地摆出来——而不是笼统地弹一个「操作失败」。

import { useCallback, useEffect, useState } from 'react'
import { ApiError, adminApi } from '../../api'
import { useAuth } from '../../auth'
import { fmtTime, isZeroTime } from '../../format'
import { ROLE_LABEL, STATUS_TEXT } from '../../types'
import type { Role, User } from '../../types'
import { Empty, Field, Loading, Modal, Notice, SecretOnce } from '../../components/ui'

export function Users() {
  const { user: me, isOwner } = useAuth()
  const [rows, setRows] = useState<User[] | null>(null)
  const [err, setErr] = useState('')
  // 策略类错误（最后一个 owner、邮箱重复）单独一格：它需要用户改做法，
  // 而不是重试。
  const [policy, setPolicy] = useState('')
  const [createOpen, setCreateOpen] = useState(false)

  const load = useCallback(async () => {
    try {
      setRows(await adminApi.listUsers())
      setErr('')
    } catch (e) {
      setErr((e as Error).message)
      setRows([])
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const patch = async (id: string, body: { role?: string; status?: string }) => {
    setPolicy('')
    setErr('')
    try {
      const next = await adminApi.updateUser(id, body)
      setRows((prev) => prev?.map((u) => (u.id === id ? next : u)) ?? null)
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setPolicy(e.message)
      else if (e instanceof ApiError && e.status === 403) setPolicy(e.message)
      else setErr((e as Error).message)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">用户</div>
          <div className="page-sub">
            本租户的成员。角色决定能看到的页面：owner 管租户，admin 管人与凭据，member 只能用工作台。
          </div>
        </div>
        <button className="btn-primary" onClick={() => setCreateOpen(true)}>
          新建用户
        </button>
      </div>

      {policy && (
        <Notice kind="warn">
          {policy}
          <div className="notice-sub">
            租户必须始终保留至少一个启用状态的 owner，否则没人能给这个租户改额度或解封。
            先把另一个用户提升为 owner，再来改这一个。
          </div>
        </Notice>
      )}
      {err && <Notice kind="error">{err}</Notice>}

      <div className="tbl-wrap">
        {rows === null ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty text="还没有用户" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th>邮箱</th>
                <th>姓名</th>
                <th style={{ width: 130 }}>角色</th>
                <th style={{ width: 90 }}>状态</th>
                <th style={{ width: 160 }}>最后登录</th>
                <th style={{ width: 90 }} />
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => {
                const self = me?.id === u.id
                return (
                  <tr key={u.id} className={u.status !== 'active' ? 'off' : ''}>
                    <td>
                      <span className="mono">{u.email}</span>
                      {self && <span className="tag">你</span>}
                    </td>
                    <td className="dim">{u.displayName || '—'}</td>
                    <td>
                      <select
                        className="select"
                        value={u.role}
                        onChange={(e) => void patch(u.id, { role: e.target.value })}
                      >
                        {/* owner 这一项始终渲染：列表里可能已经有 owner 行，
                            缺了这个 option，select 会显示成空白。
                            admin 不能授予 owner（后端 403），所以把它禁用。 */}
                        <option value="owner" disabled={!isOwner}>
                          所有者
                        </option>
                        <option value="admin">管理员</option>
                        <option value="member">成员</option>
                      </select>
                    </td>
                    <td>
                      <span className={`dot-tag ${u.status === 'active' ? 'ok' : 'off'}`}>
                        {STATUS_TEXT[u.status] || u.status}
                      </span>
                    </td>
                    <td className="dim">
                      {isZeroTime(u.lastLoginAt) ? '从未登录' : fmtTime(u.lastLoginAt)}
                    </td>
                    <td>
                      <div className="row-actions">
                        {u.status === 'active' ? (
                          <button
                            className="btn small danger"
                            onClick={() => void patch(u.id, { status: 'suspended' })}
                          >
                            停用
                          </button>
                        ) : (
                          <button
                            className="btn small"
                            onClick={() => void patch(u.id, { status: 'active' })}
                          >
                            启用
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>

      {createOpen && (
        <CreateUser
          isOwner={isOwner}
          onClose={() => setCreateOpen(false)}
          onDone={() => void load()}
        />
      )}
    </div>
  )
}

function CreateUser({
  isOwner,
  onClose,
  onDone,
}: {
  isOwner: boolean
  onClose: () => void
  onDone: () => void
}) {
  const [email, setEmail] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [role, setRole] = useState<Role>('member')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  // 后端只在「没填密码」时生成初始密码并回传；它没有存明文，
  // 关掉这个弹层就再也拿不到了，所以必须让用户确认一次。
  const [created, setCreated] = useState<{ email: string; password: string } | null>(null)
  const [ack, setAck] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    setBusy(true)
    try {
      const res = await adminApi.createUser({
        email: email.trim(),
        displayName: displayName.trim() || undefined,
        role,
        password: password || undefined,
      })
      onDone()
      if (res.initialPassword) {
        setCreated({ email: res.user.email, password: res.initialPassword })
      } else {
        onClose()
      }
    } catch (e2) {
      setErr((e2 as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (created) {
    return (
      <Modal
        title="用户已创建"
        onClose={() => {
          if (!ack) return
          onClose()
        }}
        footer={
          <div className="modal-actions">
            <button className="btn-primary" disabled={!ack} onClick={onClose}>
              关闭
            </button>
          </div>
        }
      >
        <div className="stack">
          <Notice kind="warn">
            初始密码只显示这一次，服务端只存了哈希，关掉之后无法再取回。
            请立刻把它交给 <span className="mono">{created.email}</span>，并让对方登录后自行修改。
          </Notice>
          <SecretOnce label="初始密码" value={created.password} />
          <label className="check">
            <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            我已保存这条密码
          </label>
        </div>
      </Modal>
    )
  }

  return (
    <Modal title="新建用户" onClose={onClose}>
      <form onSubmit={submit} className="stack">
        <Field label="邮箱">
          <input
            className="input"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="teammate@example.com"
            required
          />
        </Field>
        <Field label="姓名" hint="留空会用邮箱前缀">
          <input
            className="input"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </Field>
        <Field label="角色">
          <select className="select" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            {isOwner && <option value="owner">{ROLE_LABEL.owner}</option>}
            <option value="admin">{ROLE_LABEL.admin}</option>
            <option value="member">{ROLE_LABEL.member}</option>
          </select>
        </Field>
        <Field label="初始密码" hint="留空则由系统生成一个随机密码（只会显示一次）">
          <input
            className="input"
            type="text"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="留空自动生成"
            autoComplete="new-password"
          />
        </Field>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="modal-actions">
          <button className="btn" type="button" onClick={onClose}>
            取消
          </button>
          <button className="btn-primary" type="submit" disabled={busy}>
            {busy ? '创建中…' : '创建'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
