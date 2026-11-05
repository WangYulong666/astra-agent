// 用量与花费。
//
// 数字的格式不是小事：token 量动辄六位数，不千分位根本读不出量级；
// 金额在 $0.0001 量级，保留 2 位会让整页都是 0.00。

import { useCallback, useEffect, useMemo, useState } from 'react'
import { adminApi } from '../../api'
import { fmtNumber, fmtTime, fmtUsd, shortId } from '../../format'
import type { UsageReport, User } from '../../types'
import { Empty, Loading, Notice } from '../../components/ui'

type Range = '7d' | '30d' | 'month'

const RANGE_LABEL: Record<Range, string> = {
  '7d': '近 7 天',
  '30d': '近 30 天',
  month: '本月',
}

/** 后端的 from/to 只认 RFC3339 或 YYYY-MM-DD（纯日期按 UTC 0 点解释）。 */
function isoDate(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${m}-${day}`
}

function rangeStart(r: Range): string {
  const now = new Date()
  if (r === 'month') return isoDate(new Date(now.getFullYear(), now.getMonth(), 1))
  // 「近 7 天」含今天，所以减 6 而不是 7
  const from = new Date(now)
  from.setDate(from.getDate() - (r === '7d' ? 6 : 29))
  return isoDate(from)
}

export function Usage() {
  const [range, setRange] = useState<Range>('7d')
  const [data, setData] = useState<UsageReport | null>(null)
  const [err, setErr] = useState('')
  const [users, setUsers] = useState<User[]>([])

  const load = useCallback(async (r: Range) => {
    setErr('')
    try {
      setData(await adminApi.usage(rangeStart(r)))
    } catch (e) {
      setErr((e as Error).message)
      setData(null)
    }
  }, [])

  useEffect(() => {
    void load(range)
  }, [load, range])

  // 明细里只有 userId，展示时不认人。用户表通常几十条，拉一次换掉整列 ID 值得。
  useEffect(() => {
    void (async () => {
      try {
        setUsers(await adminApi.listUsers())
      } catch {
        /* 没权限就退化成显示短 ID */
      }
    })()
  }, [])

  const emailOf = useMemo(() => {
    const m = new Map<string, string>()
    for (const u of users) m.set(u.id, u.email)
    return (id?: string) => (id ? m.get(id) || shortId(id) : '—')
  }, [users])

  const sum = data?.sum

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">用量</div>
          <div className="page-sub">
            只统计网关记账的部分（source=gateway）。agent 自报的用量只做展示，不参与计费。
          </div>
        </div>
        <div className="seg">
          {(Object.keys(RANGE_LABEL) as Range[]).map((r) => (
            <button
              key={r}
              className={`seg-item ${range === r ? 'on' : ''}`}
              onClick={() => setRange(r)}
            >
              {RANGE_LABEL[r]}
            </button>
          ))}
        </div>
      </div>

      {err && <Notice kind="error">{err}</Notice>}

      <div className="stats">
        <Stat k="输入 token" v={fmtNumber(sum?.inputTokens)} />
        <Stat k="输出 token" v={fmtNumber(sum?.outputTokens)} />
        <Stat k="花费" v={fmtUsd(sum?.costMicroUsd)} />
        <Stat k="调用次数" v={fmtNumber(sum?.calls)} />
      </div>

      <div className="tbl-wrap">
        {data === null ? (
          <Loading />
        ) : data.items.length === 0 ? (
          <Empty text="这段时间没有调用记录" />
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th style={{ width: 160 }}>时间</th>
                <th>用户</th>
                <th>模型</th>
                <th className="num" style={{ width: 110 }}>输入</th>
                <th className="num" style={{ width: 110 }}>输出</th>
                <th className="num" style={{ width: 100 }}>花费</th>
                <th style={{ width: 100 }}>来源</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((it) => (
                <tr key={it.id}>
                  <td className="dim">{fmtTime(it.createdAt)}</td>
                  <td className="dim">{emailOf(it.userId)}</td>
                  <td>
                    <span className="mono small">{it.model || '—'}</span>
                  </td>
                  <td className="num">{fmtNumber(it.inputTokens)}</td>
                  <td className="num">{fmtNumber(it.outputTokens)}</td>
                  <td className="num">{fmtUsd(it.costMicroUsd)}</td>
                  <td>
                    <span className={`dot-tag ${it.source === 'gateway' ? 'ok' : 'dim'}`}>
                      {it.source}
                    </span>
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

function Stat({ k, v }: { k: string; v: string }) {
  return (
    <div className="stat">
      <div className="stat-k">{k}</div>
      <div className="stat-v">{v}</div>
    </div>
  )
}
