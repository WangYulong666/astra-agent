// 沙箱回收。
//
// 这一页要回答的是运维最常问的三个问题：回收开着吗、阈值是多少、上一轮到底动了什么。
// 全部放在一屏里，因为这三件事分开看任何一件都会得出错误结论
// （「没回收」可能是没开，也可能是开了但没到阈值）。
//
// 「立即清理」按钮是唯一有副作用的操作，且它的语义最容易被误解——
// 所以确认框里必须写清：只销毁沙箱，不删会话数据。

import { useCallback, useEffect, useState } from 'react'
import { adminApi } from '../../api'
import { fmtTime } from '../../format'
import type { LifecycleReport, LifecycleView } from '../../types'
import { Empty, Loading, Notice } from '../../components/ui'

export function Lifecycle() {
  const [data, setData] = useState<LifecycleView | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [justSwept, setJustSwept] = useState<LifecycleReport | null>(null)

  const load = useCallback(async () => {
    setErr('')
    try {
      setData(await adminApi.lifecycle())
    } catch (e) {
      setErr((e as Error).message)
      setData(null)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const sweep = async () => {
    const ok = window.confirm(
      '立即执行一轮沙箱回收？\n\n' +
        '本地会话自动回收会暂停进程并保留工作文件；孤儿或已删除会话的环境会被销毁。' +
        '历史消息和审批记录仍保留。\n\n' +
        '空闲回收会跳过未结束任务；达到最长存活时间仍会中断执行。',
    )
    if (!ok) return
    setBusy(true)
    setErr('')
    try {
      const rep = await adminApi.sweepNow()
      setJustSwept(rep)
      // 报告会写进 Reaper 的 lastSweep，重查一次让页面上的「上一轮」同步更新
      await load()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (err && !data) {
    return (
      <div className="page">
        <div className="page-head">
          <div>
            <div className="page-title">沙箱回收</div>
          </div>
        </div>
        <Notice kind="error">{err}</Notice>
      </div>
    )
  }
  if (!data) return <Loading />

  const p = data.policy
  const last = data.lastSweep

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">沙箱回收</div>
          <div className="page-sub">
            后台按周期扫描：无对应会话的孤儿、会话已结束、超过存活上限、长时间无活动的沙箱会被回收。
            本地活跃会话回收时暂停进程并保留文件；空闲回收跳过未结束任务。
          </div>
        </div>
        <button
          className="btn"
          disabled={busy || !p.enabled}
          title={p.enabled ? '' : '回收已关闭（三项阈值均为 0），此时清理不会销毁任何东西'}
          onClick={sweep}
        >
          {busy ? '清理中…' : '立即清理'}
        </button>
      </div>

      {err && <Notice kind="error">{err}</Notice>}

      {!p.enabled && (
        <Notice kind="warn">
          回收未启用：扫描周期、空闲阈值、存活上限都是 0。此时后台循环不启动，
          手动清理也不会销毁任何东西。要启用请用 -sandbox-sweep / -sandbox-idle / -sandbox-max-age 启动。
        </Notice>
      )}

      <div className="stats">
        <Stat k="扫描周期" v={p.enabled && p.sweepIntervalSeconds > 0 ? p.sweepInterval : '关闭'} />
        <Stat k="空闲回收" v={p.idleTimeoutSeconds > 0 ? p.idleTimeout : '不限'} />
        <Stat k="存活上限" v={p.maxAgeSeconds > 0 ? p.maxAge : '不限'} />
        <Stat k="每租户上限" v={p.maxPerTenant > 0 ? String(p.maxPerTenant) : '不限'} />
      </div>

      {justSwept && (
        <Notice kind="info">
          刚刚清理完成：扫描 {justSwept.scanned} 个，暂停 {justSwept.paused ?? 0} 个，销毁 {justSwept.destroyed} 个
          （孤儿 {justSwept.orphaned} / 超龄 {justSwept.expired} / 空闲 {justSwept.idle}），
          保留 {justSwept.kept} 个。
        </Notice>
      )}

      <div className="card">
        <div className="card-head">上一轮扫描</div>
        {!last ? (
          <Empty text="还没有扫描记录（后台循环尚未跑过，也没有手动触发过）" />
        ) : (
          <>
            <div className="tbl-wrap">
              <table className="tbl">
                <thead>
                  <tr>
                    <th className="num">扫描</th>
                    <th className="num">暂停</th>
                    <th className="num">销毁</th>
                    <th className="num">孤儿</th>
                    <th className="num">超龄</th>
                    <th className="num">空闲</th>
                    <th className="num">保留</th>
                    <th>开始时间</th>
                    <th>结束时间</th>
                  </tr>
                </thead>
                <tbody>
                  <tr>
                    <td className="num">{last.scanned}</td>
                    <td className="num">{last.paused ?? 0}</td>
                    <td className="num">{last.destroyed}</td>
                    <td className="num">{last.orphaned}</td>
                    <td className="num">{last.expired}</td>
                    <td className="num">{last.idle}</td>
                    <td className="num">{last.kept}</td>
                    <td className="dim">{fmtTime(last.startedAt)}</td>
                    <td className="dim">{fmtTime(last.finishedAt)}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            {last.errors.length > 0 && (
              <div className="notice warn" style={{ margin: '12px 16px' }}>
                <div>本轮有 {last.errors.length} 个沙箱处理失败（不影响其它沙箱）：</div>
                <ul style={{ margin: '8px 0 0 18px' }}>
                  {last.errors.slice(0, 8).map((e, i) => (
                    <li key={i} className="mono small">
                      {e}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </>
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
