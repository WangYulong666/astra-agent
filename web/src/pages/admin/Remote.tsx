// 远程桌面（/admin/remote）：能力条 + 设备 + 会话 + 画面查看器。
//
// 这一页的产品判断（写死在结构里，别改回去）：
//
// 1. **能力分层，不是全有或全无。** 缺辅助功能权限的机器仍然可以看画面、
//    按快捷按钮。所以三盏灯各自独立，缺权限用中性灰 + 「怎么修」折叠说明，
//    不用红色——那不是故障，是这台机器还没授权。
// 2. **不做二维码。** 画 QR 要写编码器（300 行且极易错），不引依赖就不做。
//    改成「大号 6 位码 + 一键复制深链」，手机上输入六位数比扫一个屏更省事。
// 3. **会话一律可断开。** 急停按钮固定在底部拇指可达处，任何状态下都在。

import { useCallback, useEffect, useMemo, useState } from 'react'
import { ApiError, remoteApi } from '../../api'
import type {
  RemoteCapabilities,
  RemoteDevice,
  RemotePairing,
  RemoteSession,
} from '../../api'
import { Modal, Notice, copyText } from '../../components/ui'
import { fmtTime, relTime } from '../../format'
import { RemoteViewer } from './RemoteViewer'
import {
  capabilityLamps,
  formatCountdown,
  formatDuration,
  formatFps,
  isTerminalStatus,
  modeLabel,
  sessionStatusLabel,
  sessionStatusTone,
} from './remoteModel'

/** 会话区的自动刷新：5s。远程会话状态变化不会推给这一页（WS 只给画面），
 *  轮询是唯一来源；5s 是「断开后不用手动刷新」和「别把接口打满」的折中。 */
const SESSIONS_POLL_MS = 5000

function isUnavailable(e: unknown): boolean {
  return e instanceof ApiError && e.status === 503
}

export function Remote() {
  const [cap, setCap] = useState<RemoteCapabilities | null>(null)
  const [capErr, setCapErr] = useState('')
  const [disabled, setDisabled] = useState(false)

  const [devices, setDevices] = useState<RemoteDevice[] | null>(null)
  const [devErr, setDevErr] = useState('')

  const [sessions, setSessions] = useState<RemoteSession[] | null>(null)
  const [sessErr, setSessErr] = useState('')

  const [pairing, setPairing] = useState<RemotePairing | null>(null)
  const [pairOpen, setPairOpen] = useState(false)
  const [pairErr, setPairErr] = useState('')
  const [pairBusy, setPairBusy] = useState(false)

  const [active, setActive] = useState<RemoteSession | null>(null)
  const [busy, setBusy] = useState('')
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')
  const [clock, setClock] = useState(() => Date.now())

  const loadCap = useCallback(async () => {
    setCapErr('')
    setDisabled(false)
    setCap(null)
    try {
      setCap(await remoteApi.capabilities())
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setCapErr((e as Error).message)
    }
  }, [])

  const loadDevices = useCallback(async () => {
    setDevErr('')
    try {
      const res = await remoteApi.devices()
      setDevices(res.items || [])
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setDevErr((e as Error).message)
      setDevices([])
    }
  }, [])

  const loadSessions = useCallback(async () => {
    setSessErr('')
    try {
      setSessions(await remoteApi.sessions())
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      // 拿不到列表就保留上一次的：本地刚发起的会话不该因为一次轮询失败消失。
      else setSessErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void loadCap()
    void loadDevices()
    void loadSessions()
  }, [loadCap, loadDevices, loadSessions])

  // 会话轮询。页面不可见时不打接口（手机上切后台还在轮询纯属浪费电）。
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'hidden') return
      setClock(Date.now())
      void loadSessions()
    }, SESSIONS_POLL_MS)
    return () => window.clearInterval(timer)
  }, [loadSessions])

  // 配对码倒计时：只在弹层开着时跑。
  useEffect(() => {
    if (!pairOpen) return
    const timer = window.setInterval(() => setClock(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [pairOpen])

  const lamps = capabilityLamps(cap)
  const canInput = !!cap?.input.available

  const openPairing = async () => {
    setPairBusy(true)
    setPairErr('')
    setMsg('')
    try {
      const p = await remoteApi.startPairing()
      setPairing(p)
      setPairOpen(true)
      setClock(Date.now())
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setPairErr((e as Error).message)
    } finally {
      setPairBusy(false)
    }
  }

  const startSession = async (device: RemoteDevice, mode: 'view' | 'control') => {
    setBusy(device.id + mode)
    setErr('')
    setMsg('')
    try {
      const s = await remoteApi.startSession({
        deviceId: device.id,
        mode,
        // 来源描述进审计。UA 太长，只留前半段。
        client: `web / ${navigator.userAgent.slice(0, 60)}`,
      })
      setActive(s)
      void loadSessions()
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setErr((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const revoke = async (device: RemoteDevice) => {
    const ok = window.confirm(
      `吊销「${device.name}」？\n\n这台设备会立刻失去访问权，正在看的会话同时断开；想再用必须重新配对。`,
    )
    if (!ok) return
    setBusy(device.id + 'revoke')
    try {
      await remoteApi.revokeDevice(device.id)
      await loadDevices()
      await loadSessions()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const endSession = async (s: RemoteSession) => {
    setBusy(s.id)
    try {
      await remoteApi.endSession(s.id)
      if (active?.id === s.id) setActive(null)
      await loadSessions()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  // 查看器回调必须是稳定引用：它进了查看器的轮询 effect 依赖。
  const handleSession = useCallback((s: RemoteSession) => {
    setSessions((cur) => {
      if (!cur) return [s]
      const i = cur.findIndex((x) => x.id === s.id)
      if (i < 0) return [s, ...cur]
      const next = cur.slice()
      next[i] = s
      return next
    })
  }, [])

  const handleExit = useCallback((reason: string) => {
    setActive(null)
    setMsg(
      reason === 'user_ended'
        ? '会话已断开。'
        : reason === 'denied'
          ? '电脑上拒绝了这次接管。'
          : '会话已结束。',
    )
  }, [])

  const liveSessions = useMemo(
    () => (sessions || []).filter((s) => !isTerminalStatus(s.status)),
    [sessions],
  )

  if (disabled) {
    return (
      <div className="page remote-page">
        <div className="page-head">
          <div>
            <div className="page-title">远程桌面</div>
            <div className="page-sub">
              用手机看电脑屏幕、接管鼠标键盘，或按快捷按钮把机器弄回正轨。
            </div>
          </div>
        </div>
        <div className="card">
          <div className="card-head">远程桌面未启用</div>
          <div className="card-body">
            <Notice kind="warn">
              /v1/remote/* 返回 503：控制面没有装配远程桌面模块（
              <code>ASTRA_REMOTE_DISABLED=1</code>，或启动时能力探测失败）。
              这一屏不画空列表——空列表会被读成「配对过了但设备掉了」。
              <div className="notice-sub">
                <button className="btn small" onClick={() => void loadCap()}>
                  重新检查
                </button>
              </div>
            </Notice>
            <div className="settings-kv">
              <div className="settings-kv-row">
                <div className="settings-kv-key mono">ASTRA_REMOTE_DISABLED</div>
                <div className="settings-kv-val dim small">
                  设为 1 时全局关闭远程桌面（含已有会话的急停）。去掉它并重启控制面。
                </div>
              </div>
              <div className="settings-kv-row">
                <div className="settings-kv-key mono">/v1/remote/capabilities</div>
                <div className="settings-kv-val dim small">
                  直接访问它能看到分层的探测结果：监看 / 快捷操作 / 键鼠接管各自缺什么。
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="page remote-page">
      <div className="page-head">
        <div>
          <div className="page-title">远程桌面</div>
          <div className="page-sub">
            手机看电脑、必要时接管。能力分三层，缺权限的那层会明确告诉你缺什么——
            不影响其它层继续用。
          </div>
        </div>
        <button className="btn page-act" onClick={() => void openPairing()} disabled={pairBusy}>
          {pairBusy ? '生成中…' : '添加设备'}
        </button>
      </div>

      {msg && <Notice kind="info">{msg}</Notice>}
      {err && (
        <Notice kind="error">
          {err}
          <div className="notice-sub">
            <button className="btn small" onClick={() => setErr('')}>
              知道了
            </button>
          </div>
        </Notice>
      )}
      {pairErr && <Notice kind="error">生成配对码失败：{pairErr}</Notice>}

      {/* ── 1. 能力条 ─────────────────────────────────────── */}
      <div className="remote-caps">
        <div className="remote-caps-head">
          <span className="remote-caps-title">本机能力</span>
          {cap && (
            <span className="dim small">
              {cap.platform} · {cap.screen.width}×{cap.screen.height} · 上限 {cap.maxFps}fps
              {cap.capture.backend ? ` · ${cap.capture.backend}` : ''}
            </span>
          )}
          {!cap && !capErr && <span className="dim small">探测中…</span>}
        </div>
        {capErr && (
          <Notice kind="error">
            读取能力失败：{capErr}
            <div className="notice-sub">
              <button className="btn small" onClick={() => void loadCap()}>
                重试
              </button>
            </div>
          </Notice>
        )}
        <div className="remote-lamps">
          {lamps.map((l) => (
            <div key={l.key} className={`remote-lamp ${l.on ? 'on' : 'off'}`}>
              <span className="remote-lamp-dot" aria-hidden="true" />
              <div className="remote-lamp-text">
                <div className="remote-lamp-label">
                  {l.label}
                  <span className="remote-lamp-state">{l.on ? '可用' : '不可用'}</span>
                </div>
                <div className="remote-lamp-what">{l.on ? l.what : l.reason || l.what}</div>
              </div>
            </div>
          ))}
        </div>

        {cap?.notice && <div className="remote-caps-notice">{cap.notice}</div>}

        {/* 缺权限时的「怎么修」：折叠着，不挡路；展开后是逐条可执行的动作。 */}
        {lamps.some((l) => !l.on && l.hint) && (
          <details className="remote-fix">
            <summary>某一层不可用？看看怎么修</summary>
            <div className="remote-fix-body">
              {lamps
                .filter((l) => !l.on)
                .map((l) => (
                  <div key={l.key} className="remote-fix-row">
                    <div className="remote-fix-key">{l.label}</div>
                    <div className="remote-fix-val">
                      <div>{l.reason || '未探测到可用后端'}</div>
                      {l.hint && <div className="dim small">{l.hint}</div>}
                    </div>
                  </div>
                ))}
              <p className="dim small">
                权限改完要重启控制面（进程继承的是启动时的 TCC 授权）。
                只有「监看」不可用时，通常是屏幕录制权限；只有「键鼠接管」不可用时，
                是辅助功能权限。
              </p>
            </div>
          </details>
        )}
      </div>

      <div className={`remote-grid ${active ? 'with-viewer' : ''}`}>
        <div className="remote-side">
          {/* ── 2. 设备 ─────────────────────────────────── */}
          <div className="card remote-card">
            <div className="card-head">
              设备
              {devices && <span className="remote-count">{devices.length}</span>}
            </div>
            {devErr && (
              <div className="card-body">
                <Notice kind="error">
                  读取设备失败：{devErr}
                  <div className="notice-sub">
                    <button className="btn small" onClick={() => void loadDevices()}>
                      重试
                    </button>
                  </div>
                </Notice>
              </div>
            )}
            {devices === null && !devErr && (
              <div className="card-body dim small">读取中…</div>
            )}
            {devices && devices.length === 0 && !devErr && (
              <div className="card-body">
                <div className="remote-empty">
                  还没有配对过设备。点右上角「添加设备」拿到 6 位配对码，
                  在手机的 Astra App 里输入即可。
                </div>
              </div>
            )}
            {devices && devices.length > 0 && (
              <div className="remote-devices">
                {devices.map((d) => {
                  const revoked = !!d.revokedAt
                  return (
                    <div key={d.id} className={`remote-device ${revoked ? 'off' : ''}`}>
                      <div className="remote-device-top">
                        <span className="remote-device-title" title={d.name}>
                          {d.name}
                        </span>
                        <span className={`remote-dot ${d.online ? 'on' : ''}`} aria-hidden="true" />
                        <span className="remote-device-state">{d.online ? '在线' : '离线'}</span>
                      </div>
                      <div className="remote-device-meta">
                        <span>{d.platform || '未知平台'}</span>
                        <span>最后活跃 {relTime(d.lastSeenAt) || '—'}</span>
                        {revoked && <span className="remote-tag off">已吊销</span>}
                      </div>
                      {!revoked && (
                        <div className="remote-device-actions">
                          <button
                            className="btn small"
                            disabled={busy === d.id + 'view'}
                            onClick={() => void startSession(d, 'view')}
                            title="只看画面，不发任何键鼠事件"
                          >
                            连接（只看）
                          </button>
                          <button
                            className="btn small"
                            disabled={busy === d.id + 'control' || !canInput}
                            onClick={() => void startSession(d, 'control')}
                            title={
                              canInput
                                ? '接管鼠标键盘，需要电脑上点确认'
                                : `本机不能注入键鼠：${cap?.input.reason || '缺少辅助功能权限'}`
                            }
                          >
                            接管
                          </button>
                          <button
                            className="btn small danger"
                            disabled={busy === d.id + 'revoke'}
                            onClick={() => void revoke(d)}
                          >
                            吊销
                          </button>
                        </div>
                      )}
                      {revoked && (
                        <div className="remote-device-actions dim small">
                          已吊销于 {fmtTime(d.revokedAt)}，重新配对后会以新设备出现。
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </div>

          {/* ── 3. 进行中的会话 ─────────────────────────── */}
          <div className="card remote-card">
            <div className="card-head">
              进行中的会话
              <span className="remote-count">{liveSessions.length}</span>
            </div>
            {sessErr && (
              <div className="card-body">
                <Notice kind="warn">
                  会话列表暂时取不到：{sessErr}
                  <div className="notice-sub">
                    <button className="btn small" onClick={() => void loadSessions()}>
                      重试
                    </button>
                  </div>
                </Notice>
              </div>
            )}
            {liveSessions.length === 0 && !sessErr && (
              <div className="card-body">
                <div className="remote-empty">现在没有进行中的会话。</div>
              </div>
            )}
            {liveSessions.length > 0 && (
              <div className="remote-sessions">
                {liveSessions.map((s) => {
                  return (
                    <div key={s.id} className="remote-session">
                      <div className="remote-session-top">
                        <span className="remote-device-title">{s.device || s.deviceId}</span>
                        <span className={`remote-tag ${sessionStatusTone(s.status)}`}>
                          {sessionStatusLabel(s.status)}
                        </span>
                        <span className="remote-tag dim">{modeLabel(s.mode)}</span>
                      </div>
                      <div className="remote-session-meta">
                        <span>{formatFps(s.stats?.fps)} fps</span>
                        <span>
                          {s.stats?.width ? `${s.stats.width}×${s.stats.height}` : '—'}
                        </span>
                        <span>掉帧 {s.stats?.dropped ?? 0}</span>
                        <span>时长 {formatDuration(clock - new Date(s.createdAt).getTime())}</span>
                      </div>
                      <div className="remote-session-actions">
                        <button
                          className="btn small"
                          onClick={() => setActive(s)}
                          disabled={active?.id === s.id}
                        >
                          {active?.id === s.id ? '正在查看' : '打开画面'}
                        </button>
                        <button
                          className="btn small danger"
                          disabled={busy === s.id}
                          onClick={() => void endSession(s)}
                        >
                          断开
                        </button>
                      </div>
                    </div>
                  )
                })}
              </div>
            )}
          </div>
        </div>

        <div className="remote-main">
          {active ? (
            <RemoteViewer
              initial={active}
              capabilities={cap}
              onSession={handleSession}
              onExit={handleExit}
            />
          ) : (
            <div className="card remote-placeholder">
              <div className="card-body">
                <div className="remote-placeholder-title">选一台设备开始</div>
                <div className="dim small">
                  「连接（只看）」不发任何输入，适合先确认屏幕上发生了什么；
                  「接管」会先让电脑上确认一次，批准后才能点、拖、打字。
                </div>
                <div className="remote-hints">
                  <div>
                    <b>单指轻点</b> 左键 · <b>双击</b> 双击进入
                  </div>
                  <div>
                    <b>长按 0.6 秒后拖动</b> 拖拽 · <b>双指轻点</b> 右键
                  </div>
                  <div>
                    <b>双指上下滑</b> 滚轮 · <b>捏合</b> 只放大本地画面
                  </div>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>

      {pairOpen && pairing && (
        <PairingModal
          pairing={pairing}
          now={clock}
          onClose={() => {
            setPairOpen(false)
            setPairing(null)
          }}
          onRegenerate={() => void openPairing()}
          onClaimed={async () => {
            setPairOpen(false)
            setPairing(null)
            setMsg('已添加设备。现在可以在设备列表里发起连接。')
            await loadDevices()
          }}
        />
      )}
    </div>
  )
}

/**
 * 配对弹层：大号 6 位码 + 倒计时 + 一键复制深链。
 *
 * 为什么不画二维码：编码器要 300 行、纠错等级一错就扫不出来，而且这一屏
 * 的主要用户是「人就在电脑前、手机在手边」——看一眼六位数照着手输更快。
 */
function PairingModal({
  pairing,
  now,
  onClose,
  onRegenerate,
  onClaimed,
}: {
  pairing: RemotePairing
  now: number
  onClose: () => void
  onRegenerate: () => void
  onClaimed: () => void
}) {
  const [copied, setCopied] = useState('')
  const [code, setCode] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const left = new Date(pairing.expiresAt).getTime() - now
  const expired = !(left > 0)

  const copy = async (what: 'code' | 'link', value: string) => {
    const ok = await copyText(value)
    setCopied(ok ? what : '')
    if (!ok) setErr('复制失败：这个地址不是安全上下文（http），请手动选中复制。')
    window.setTimeout(() => setCopied(''), 1800)
  }

  // 手机认领（同一浏览器里调试用）：真实场景是手机 App 调这个接口。
  const claim = async () => {
    if (!/^\d{6}$/.test(code)) {
      setErr('配对码是 6 位数字')
      return
    }
    setBusy(true)
    setErr('')
    try {
      await remoteApi.claimPairing({ code, platform: navigator.platform || 'web' })
      onClaimed()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const link = pairing.deepLink || pairing.payload

  return (
    <Modal
      title="添加设备"
      onClose={onClose}
      // 共享 Modal 的 width 是内联的，而 .modal 的 max-width:100% 是相对
      // grid 的 auto 轨道算的（等于自身宽度），手机上 480 会把弹层撑出屏幕。
      // 这里按视口收窄，不动公共样式。
      width={Math.min(480, Math.max(300, window.innerWidth - 48))}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            关闭
          </button>
          <button className="btn" onClick={onRegenerate} disabled={!expired}>
            {expired ? '重新生成' : '重新生成（先用完这张）'}
          </button>
        </>
      }
    >
      <div className="remote-pair">
        <div className={`remote-code ${expired ? 'off' : ''}`}>
          {pairing.code.slice(0, 3)} {pairing.code.slice(3)}
        </div>
        <div className="remote-pair-count">
          {expired ? (
            <span className="remote-tag off">已过期</span>
          ) : (
            <>
              还有 <b>{formatCountdown(left)}</b> 失效
            </>
          )}
          <span className="dim small"> · 一次性，只能用一次</span>
        </div>

        <ol className="remote-pair-steps">
          <li>手机打开 Astra App → 远程桌面 → 添加设备。</li>
          <li>输入上面这 6 位数字，电脑就出现在设备列表里。</li>
          <li>
            也可以复制深链在手机上直接打开：
            <code className="mono remote-deeplink">{link || '（服务端未返回深链）'}</code>
          </li>
        </ol>

        <div className="remote-pair-actions">
          <button className="btn" onClick={() => void copy('code', pairing.code)}>
            {copied === 'code' ? '已复制' : '复制配对码'}
          </button>
          <button
            className="btn"
            disabled={!link}
            onClick={() => void copy('link', link)}
          >
            {copied === 'link' ? '已复制' : '复制深链'}
          </button>
        </div>

        <div className="dim small">
          这一屏不出二维码：扫码入口在手机 App 里（相机 / 扫一扫）。
          电脑端只负责给出这串码——它只证明「人在电脑跟前」，不代表任何权限。
        </div>

        <details className="remote-claim">
          <summary>同一浏览器里认领（调试用）</summary>
          <div className="remote-claim-body">
            <input
              className="input slim"
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
              placeholder="6 位配对码"
              inputMode="numeric"
              aria-label="配对码"
            />
            <button className="btn small" onClick={() => void claim()} disabled={busy}>
              认领
            </button>
          </div>
        </details>

        {err && <Notice kind="error">{err}</Notice>}
      </div>
    </Modal>
  )
}
