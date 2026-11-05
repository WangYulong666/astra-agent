// API Key。
//
// 明文只在创建响应里出现一次（库里只存 sha256），所以创建成功后必须把
// 用户按在弹层里保存一次，不能「创建完直接关掉、再让他去列表里找」。

import { useCallback, useEffect, useState } from 'react'
import { adminApi } from '../../api'
import { fmtTime, isZeroTime } from '../../format'
import type { APIKey } from '../../types'
import { Empty, Field, Loading, Modal, Notice, SecretOnce } from '../../components/ui'

function keyState(k: APIKey): { text: string; cls: string } {
  if (!isZeroTime(k.revokedAt)) return { text: '已撤销', cls: 'off' }
  if (!isZeroTime(k.expiresAt) && new Date(k.expiresAt as string).getTime() < Date.now()) {
    return { text: '已过期', cls: 'off' }
  }
  return { text: '有效', cls: 'ok' }
}

export function Keys() {
  const [rows, setRows] = useState<APIKey[] | null>(null)
  const [err, setErr] = useState('')
  const [open, setOpen] = useState(false)

  const load = useCallback(async () => {
    try {
      setRows(await adminApi.listKeys())
      setErr('')
    } catch (e) {
      setErr((e as Error).message)
      setRows([])
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const revoke = async (k: APIKey) => {
    // 撤销不可逆（没有「恢复」，只能重新签发），所以二次确认里要说清后果。
    const ok = window.confirm(
      `撤销「${k.name}」？\n\n用这把 key 的脚本和 CI 会立刻开始 401，只能重新签发一把。`,
    )
    if (!ok) return
    try {
      await adminApi.revokeKey(k.id)
      await load()
    } catch (e) {
      setErr((e as Error).message)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">API Key</div>
          <div className="page-sub">
            给脚本和 CI 用的长期凭据，等价于「以某个用户的身份调用平台」。按需签发，用完就撤。
          </div>
        </div>
        <button className="btn-primary" onClick={() => setOpen(true)}>
          签发新 Key
        </button>
      </div>

      {err && <Notice kind="error">{err}</Notice>}

      <div className="tbl-wrap">
        {rows === null ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty text="还没有签发过 Key" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th>名称</th>
                <th style={{ width: 200 }}>前缀</th>
                <th style={{ width: 90 }}>状态</th>
                <th style={{ width: 160 }}>创建时间</th>
                <th style={{ width: 160 }}>最后使用</th>
                <th style={{ width: 90 }} />
              </tr>
            </thead>
            <tbody>
              {rows.map((k) => {
                const st = keyState(k)
                const dead = st.cls === 'off'
                return (
                  <tr key={k.id} className={dead ? 'off' : ''}>
                    <td>{k.name}</td>
                    <td>
                      <span className="mono">{k.prefix}…</span>
                    </td>
                    <td>
                      <span className={`dot-tag ${st.cls}`}>{st.text}</span>
                    </td>
                    <td className="dim">{fmtTime(k.createdAt)}</td>
                    <td className="dim">
                      {isZeroTime(k.lastUsedAt) ? '从未使用' : fmtTime(k.lastUsedAt)}
                    </td>
                    <td>
                      <div className="row-actions">
                        {!dead && (
                          <button
                            className="btn small danger"
                            onClick={() => void revoke(k)}
                          >
                            撤销
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

      {open && (
        <CreateKey
          onClose={() => setOpen(false)}
          onDone={() => void load()}
        />
      )}
    </div>
  )
}

function CreateKey({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [days, setDays] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [plain, setPlain] = useState('')
  const [ack, setAck] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    setBusy(true)
    try {
      const res = await adminApi.createKey({
        name: name.trim(),
        // 0 / 空 = 不过期。后端的语义是 ExpiresInDays <= 0 就不设过期时间。
        expiresInDays: days ? Number(days) : 0,
      })
      onDone()
      setPlain(res.plaintext)
    } catch (e2) {
      setErr((e2 as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (plain) {
    return (
      <Modal
        title="Key 已签发"
        width={520}
        onClose={() => {
          if (ack) onClose()
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
            明文只显示这一次。服务端只保存了它的哈希，关掉之后无法再查看——只能撤销重发。
          </Notice>
          <SecretOnce label="API Key" value={plain} />
          <div className="hint-line">
            用法：<span className="mono">curl -H "Authorization: Bearer &lt;key&gt;" /v1/sessions</span>
          </div>
          <label className="check">
            <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            我已保存这串 Key
          </label>
        </div>
      </Modal>
    )
  }

  return (
    <Modal title="签发新 Key" onClose={onClose}>
      <form onSubmit={submit} className="stack">
        <Field label="名称" hint="用于在列表里区分是哪把钥匙，比如 ci-deploy">
          <input
            className="input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </Field>
        <Field label="有效期（天）" hint="留空表示不过期">
          <input
            className="input"
            type="number"
            min={0}
            value={days}
            onChange={(e) => setDays(e.target.value)}
            placeholder="不过期"
          />
        </Field>
        {err && <Notice kind="error">{err}</Notice>}
        <div className="modal-actions">
          <button className="btn" type="button" onClick={onClose}>
            取消
          </button>
          <button className="btn-primary" type="submit" disabled={busy}>
            {busy ? '签发中…' : '签发'}
          </button>
        </div>
      </form>
    </Modal>
  )
}
