// 沙箱管理面。
//
// 强杀是这一页唯一有副作用的操作，而它的语义最容易被误解——「杀了之后对话还在吗」
// 是每次都会有人问的问题，所以确认框里必须把边界写死：只销毁运行环境，不动会话数据。

import { useCallback, useEffect, useMemo, useState } from 'react'
import { adminApi } from '../../api'
import { fmtTime, shortId } from '../../format'
import type { SandboxView, User } from '../../types'
import { Empty, Loading, Notice } from '../../components/ui'

export function Sandboxes() {
  const [rows, setRows] = useState<SandboxView[] | null>(null)
  const [users, setUsers] = useState<User[]>([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')

  const load = useCallback(async () => {
    setErr('')
    try {
      setRows(await adminApi.listSandboxes())
    } catch (e) {
      setErr((e as Error).message)
      setRows([])
    }
  }, [])

  useEffect(() => {
    void load()
    void (async () => {
      try {
        setUsers(await adminApi.listUsers())
      } catch {
        /* 拿不到用户表就退化成短 ID，不影响强杀 */
      }
    })()
  }, [load])

  const emailOf = useMemo(() => {
    const m = new Map<string, string>()
    for (const u of users) m.set(u.id, u.email)
    return (id?: string) => (id ? m.get(id) || shortId(id) : '—')
  }, [users])

  const kill = async (s: SandboxView) => {
    const ok = window.confirm(
      `强杀沙箱 ${shortId(s.id)}？\n\n` +
        `只销毁沙箱，不删除会话数据：历史消息、工具调用与审批记录都还在，` +
        `下次发指令会重新起一个沙箱。\n\n` +
        `如果 agent 正在跑，这一轮会立刻中断。`,
    )
    if (!ok) return
    setBusy(s.id)
    try {
      await adminApi.killSandbox(s.id)
      // 列表靠 provider 实时返回，杀完必须重查才知道真实状态
      await load()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">沙箱</div>
          <div className="page-sub">
            provider 当前的运行环境。卡死、占着端口下不来的，从这里强杀；会话记录不会因此丢失。
          </div>
        </div>
        <button className="btn" onClick={() => void load()}>
          刷新
        </button>
      </div>

      {err && <Notice kind="error">{err}</Notice>}

      <div className="tbl-wrap">
        {rows === null ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty text="当前没有运行中的沙箱" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th style={{ width: 190 }}>沙箱 ID</th>
                <th>对应会话</th>
                <th style={{ width: 110 }}>状态</th>
                <th style={{ width: 200 }}>所属用户</th>
                <th style={{ width: 160 }}>创建时间</th>
                <th style={{ width: 90 }} />
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => (
                <tr key={s.id}>
                  <td>
                    <span className="mono small" title={s.id}>
                      {shortId(s.id)}
                    </span>
                  </td>
                  <td>
                    {s.sessionId ? (
                      <span title={s.sessionId}>
                        {s.sessionTitle || '未命名会话'}
                        <span className="dim mono small"> · {shortId(s.sessionId)}</span>
                      </span>
                    ) : (
                      <span className="dim">无归属会话</span>
                    )}
                  </td>
                  <td>
                    <span className={`dot-tag ${s.state === 'running' ? 'ok' : 'dim'}`}>
                      {s.state}
                    </span>
                  </td>
                  <td className="dim">{emailOf(s.userId)}</td>
                  <td className="dim">{fmtTime(s.createdAt)}</td>
                  <td>
                    <div className="row-actions">
                      <button
                        className="btn small danger"
                        disabled={busy === s.id}
                        onClick={() => void kill(s)}
                      >
                        {busy === s.id ? '强杀中…' : '强杀'}
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
