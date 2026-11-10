// 远程桌面的纯逻辑层：坐标换算、画质档位、状态文案、重连退避。
//
// 为什么单独抽一个文件：这几件事都是「算错了也照样跑」的类型——坐标偏 2px、
// 画质档位记反、重连退避写成固定 1s，界面上都看不出来，只有真机上手才别扭。
// 抽成纯函数就能在 node 环境下逐条断言（vitest 不引 jsdom），
// 组件里只剩「什么时候调用」。
//
// 这里不 import api.ts：类型用结构化最小定义，避免测试为了一个类型把整个
// 网络层（及其对 window 的假设）拉进来。

export type RectLike = { left: number; top: number; width: number; height: number }

/** 归一化后的坐标：0..1，相对「画面」而不是相对屏幕。 */
export type NormPoint = { x: number; y: number }

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0
  return v < 0 ? 0 : v > 1 ? 1 : v
}

/** 保留 4 位：JSON 里 0.12345678 这种精度对注入没有意义，只是白占带宽。 */
function round4(v: number): number {
  return Math.round(v * 1e4) / 1e4
}

/**
 * 把浏览器坐标换成 0..1 的归一化坐标。
 *
 * `rect` 是 **getBoundingClientRect() 的结果**，不是 canvas 的 width/height：
 * canvas 内部尺寸是帧的像素（可能 1600×900），CSS 尺寸是屏幕上那一条（可能 393×221），
 * 两者差一个缩放系数。用 rect 就自动带上了 CSS 缩放和页面偏移；
 * 用 width/height 会让触摸点整体偏移。
 *
 * 捏合放大时 canvas 上有 transform: scale()，getBoundingClientRect 返回的是
 * 变换后的矩形，所以这条换算在缩放状态下依然成立。
 */
export function normalizePoint(clientX: number, clientY: number, rect: RectLike): NormPoint {
  if (!(rect.width > 0) || !(rect.height > 0)) return { x: 0, y: 0 }
  return {
    x: round4(clamp01((clientX - rect.left) / rect.width)),
    y: round4(clamp01((clientY - rect.top) / rect.height)),
  }
}

// ── 画质档位 ────────────────────────────────────────────────────

export type QualityTier = 'saver' | 'low' | 'medium' | 'high'

export type QualityPreset = {
  key: QualityTier
  label: string
  maxWidth: number
  fps: number
  quality: number
}

/**
 * 四档不是「随便取值」：480/4fps/q40 是 4G 按流量计费时的下限（单帧 ~15KB，
 * 一分钟不到 4MB），640/6 是移动网络不卡的下限，960/8 是默认（看代码能认清字），
 * 1600/12 只在局域网/家用 Wi-Fi 用。
 *
 * 「省流量」不是给 WebRTC 省——**DataChannel 里跑的仍是 JPEG**，WebRTC 买到的是
 * 直连与拥塞控制，不是带宽（见 docs/remote-desktop.md）。想省流量只能降这一档。
 *
 * 数值同时与服务端 Policy 的默认值对齐，切档时整包发过去，不做增量。
 */
export const QUALITY_PRESETS: readonly QualityPreset[] = [
  { key: 'saver', label: '省流量', maxWidth: 480, fps: 4, quality: 40 },
  { key: 'low', label: '低', maxWidth: 640, fps: 6, quality: 45 },
  { key: 'medium', label: '中', maxWidth: 960, fps: 8, quality: 60 },
  { key: 'high', label: '高', maxWidth: 1600, fps: 12, quality: 75 },
] as const

export const DEFAULT_TIER: QualityTier = 'medium'

export function qualityPreset(tier: QualityTier): QualityPreset {
  return (
    QUALITY_PRESETS.find((p) => p.key === tier) ??
    QUALITY_PRESETS.find((p) => p.key === DEFAULT_TIER) ??
    QUALITY_PRESETS[0]
  )
}

/** 发 `{"type":"quality"}` 用的载荷。字段名与后端 Quality 结构逐字对应。 */
export function qualityMessage(tier: QualityTier): {
  type: 'quality'
  maxWidth: number
  fps: number
  quality: number
} {
  const p = qualityPreset(tier)
  return { type: 'quality', maxWidth: p.maxWidth, fps: p.fps, quality: p.quality }
}

// ── 快捷键条 ────────────────────────────────────────────────────
//
// 键名必须是后端白名单里的值（internal/remotedesk/input.go 的 allowedNamedKeys
// 与「单字符 [A-Za-z0-9]」规则），否则整条事件会被 ErrDenied 丢掉：
//   Enter / Tab / Escape / Backspace / Delete / Space / ArrowUp…
// 组合键走 mods，取值 cmd / ctrl / alt / shift，逗号分隔。
//
// Cmd+C / Cmd+V 为什么不是「复制粘贴一段文本」：剪贴板不过这条通道（设计如此），
// 所以这里只发「按下 cmd+c」，粘贴的内容仍是电脑自己剪贴板里的东西。

export type KeyShortcut = {
  label: string
  key: string
  mods?: string
  /** 悬停说明。触摸设备上看不到，但桌面上能救一次误点。 */
  title?: string
}

export const KEY_SHORTCUTS: readonly KeyShortcut[] = [
  { label: 'Enter', key: 'Enter', title: '回车' },
  { label: 'Esc', key: 'Escape', title: 'Esc' },
  { label: 'Tab', key: 'Tab', title: 'Tab' },
  { label: '⌫', key: 'Backspace', title: '退格' },
  { label: '↑', key: 'ArrowUp', title: '方向上' },
  { label: '↓', key: 'ArrowDown', title: '方向下' },
  { label: '←', key: 'ArrowLeft', title: '方向左' },
  { label: '→', key: 'ArrowRight', title: '方向右' },
  { label: '⌘C', key: 'c', mods: 'cmd', title: '复制（电脑上的剪贴板）' },
  { label: '⌘V', key: 'v', mods: 'cmd', title: '粘贴（电脑上的剪贴板）' },
  { label: '⌘Z', key: 'z', mods: 'cmd', title: '撤销' },
  { label: '⌘Tab', key: 'Tab', mods: 'cmd', title: '切换应用' },
] as const

// ── 状态文案 ────────────────────────────────────────────────────

export type SessionStatus = 'pending_approval' | 'active' | 'ended' | 'denied' | 'expired'

export type StatusTone = 'ok' | 'wait' | 'off'

const STATUS_META: Record<SessionStatus, { label: string; tone: StatusTone }> = {
  // pending 用「等」而不是「挂起」：这一屏的读者是拿着手机的人，
  // 他需要知道的是「现在要去电脑上点一下」。
  pending_approval: { label: '等待电脑确认', tone: 'wait' },
  active: { label: '进行中', tone: 'ok' },
  ended: { label: '已结束', tone: 'off' },
  denied: { label: '已被拒绝', tone: 'off' },
  expired: { label: '已过期', tone: 'off' },
}

const TERMINAL: readonly string[] = ['ended', 'denied', 'expired']

export function sessionStatusLabel(status: string): string {
  return STATUS_META[status as SessionStatus]?.label ?? status ?? '未知'
}

export function sessionStatusTone(status: string): StatusTone {
  return STATUS_META[status as SessionStatus]?.tone ?? 'off'
}

/** 终态：不再轮询、不再重连。 */
export function isTerminalStatus(status: string): boolean {
  return TERMINAL.includes(status)
}

export function modeLabel(mode: string): string {
  return mode === 'view' ? '只看' : '接管'
}

// ── 连接与统计文案 ──────────────────────────────────────────────

/**
 * 重连退避：1s / 2s / 4s / 8s / 16s，共 5 次。
 *
 * 返回 null 表示「不再重试」。写成「第 n 次等多少」而不是在组件里累乘，
 * 是为了让测试能直接断言这串数字——退避写错时，症状是「手机锁屏回来一片黑」，
 * 现场很难判断到底是退避太长还是压根没重连。
 */
export const RECONNECT_DELAYS: readonly number[] = [1000, 2000, 4000, 8000, 16000]

export function reconnectDelay(attempt: number): number | null {
  if (!Number.isInteger(attempt) || attempt < 0) return null
  return attempt < RECONNECT_DELAYS.length ? RECONNECT_DELAYS[attempt] : null
}

/** 会话时长：秒级足够，超过一小时才加小时段。 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const total = Math.floor(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`
}

/** 配对码倒计时：超过 60 秒只显示分秒，避免「01:59」和「00:59」跳宽度。 */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000))
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

export function formatFps(fps: number | undefined): string {
  if (!fps || !Number.isFinite(fps) || fps <= 0) return '—'
  return fps.toFixed(1)
}

/** 掉帧率：分母用 frames + dropped，否则「收到 0 帧时掉帧 100%」会误报。 */
export function dropRate(frames: number | undefined, dropped: number | undefined): string {
  const f = frames ?? 0
  const d = dropped ?? 0
  const total = f + d
  if (total <= 0) return '—'
  const pct = (d / total) * 100
  return pct >= 10 ? `${pct.toFixed(0)}%` : `${pct.toFixed(1)}%`
}

export function resolutionText(w?: number, h?: number): string {
  if (!w || !h) return '—'
  return `${w}×${h}`
}

// ── 能力分层 ────────────────────────────────────────────────────

export type CapabilityLike = { available: boolean; backend?: string; reason?: string; hint?: string }

export type CapabilityLamps = {
  platform?: string
  capture: CapabilityLike
  actions: CapabilityLike
  input: CapabilityLike
  notice?: string
}

export type Lamp = {
  key: 'capture' | 'actions' | 'input'
  label: string
  /** 一句话说明这一层能干什么。 */
  what: string
  on: boolean
  reason: string
  hint: string
}

/**
 * 三盏灯。**不区分「错误」和「正常」的红绿语义**——缺权限不是故障，
 * 是这台机器的配置状态，用中性灰比用红色更诚实（红色会让人以为要重启服务）。
 * 只有「可用 / 不可用」两态，具体原因交给展开后的「怎么修」。
 */
export function capabilityLamps(cap: CapabilityLamps | null): Lamp[] {
  const layer = (
    key: Lamp['key'],
    label: string,
    what: string,
    c: CapabilityLike | undefined,
  ): Lamp => ({
    key,
    label,
    what,
    on: !!c?.available,
    reason: c?.reason || (c ? '' : '服务端没有返回这一层的能力信息'),
    hint: c?.hint || '',
  })

  return [
    layer('capture', '监看', '看实时画面（JPEG 帧）', cap?.capture),
    layer('actions', '快捷操作', '截屏 / 锁屏 / 打开应用或网址', cap?.actions),
    layer('input', '键鼠接管', '点击、拖拽、滚轮、键盘', cap?.input),
  ]
}

/**
 * 只读原因。空串 = 可以控。
 *
 * 三种只读要分开说：用户主动选了「只看」、服务端没给键鼠权限、
 * 以及能力状态压根没拿到（旧二进制/接口 503）。混成一句「只读模式」
 * 会让人以为是自己的操作错了。
 */
export function readOnlyReason(cap: CapabilityLike | null, mode: string): string {
  if (mode === 'view') return '这是「只看」会话：不发送任何键鼠事件。'
  if (!cap) return '没拿到本机的键鼠能力状态，先按只读处理。'
  if (!cap.available) {
    const why = cap.reason || '本机未授予辅助功能权限'
    return `当前只能观看：${why}`
  }
  return ''
}

/** 快捷操作能不能用；不能用时给出原因，用于按钮 title。 */
export function actionsBlockedReason(cap: CapabilityLike | null): string {
  if (!cap) return '能力状态未知'
  if (!cap.available) return cap.reason || '本机不支持快捷操作'
  return ''
}
