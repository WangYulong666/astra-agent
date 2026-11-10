// 链路判定：「直连」还是「经中继」。
//
// 为什么需要这个文件：v2 引入 relay 之后，用户看到的地址可能有两种——
//   1. 局域网/本机直连：http://192.168.1.20:7788/…
//   2. 经公网中继：    https://relay.example.com/t/<device>/…
// 这两种情况的排障路径完全不同（直连连不上看防火墙，中继连不上看隧道）。
// 界面必须说清楚「你现在走的是哪条」，否则用户会把「relay 没连上」当成
// 「远程桌面坏了」。
//
// 判定不猜域名：只看路径里有没有 relay 的设备前缀 `/t/<device>/`，
// 以及 GET /v1/remote/tunnel 返回的 url 的 host 是不是当前页面 host。
// 猜「host 里带 relay 字样」在自定义域名下必然错。
//
// 这个文件不碰 DOM：location 由调用方传进来（浏览器里是 window.location，
// 单测里是普通对象），所以可以在 node 里跑。

export type LinkLocation = {
  protocol: string
  host: string
  pathname: string
}

export type TunnelState = {
  /** 是否配置了 ASTRA_RELAY_URL。未配置时 relay 一律不可用。 */
  configured: boolean
  connected: boolean
  /** relay 的公网地址，例如 https://relay.example.com（可能为空）。 */
  url: string
  /** 设备 id：手机访问路径里的 /t/<device>。 */
  deviceId: string
  /** 在途逻辑流数量。 */
  streams: number
  /** 断了重连过几次。持续增长说明网络不稳。 */
  reconnects: number
  lastError: string
}

export type LinkKind = 'direct' | 'relay'

export type LinkTone = 'ok' | 'warn' | 'off'

export type LinkReport = {
  kind: LinkKind
  tone: LinkTone
  /** 短标签，给状态色块用：直连 / 经中继 / 未知。 */
  label: string
  /** 一句话主文案。 */
  summary: string
  /** 细节行（设备 id、重连次数、隧道地址等），可能为空。 */
  details: string[]
  /** 需要用户去做点什么时给一句（未配置中继、隧道断开）。 */
  hint?: string
}

/** 未配置 relay 时的固定文案：与文档 §外网怎么连 对齐。 */
export const NO_RELAY_HINT = '未配置外网中继：手机只能在同一局域网访问'

const RELAY_PREFIX = /^\/t\/([^/]+)(\/|$)/

/**
 * 从 URL 路径里取 relay 的 device id。不是 /t/<device>/ 就返回空串。
 */
export function relayDeviceFromPath(pathname: string): string {
  const m = RELAY_PREFIX.exec(pathname || '')
  return m ? decodeURIComponent(m[1]) : ''
}

/** 页面自己是不是被 relay 转发出来的（路径带 /t/<device>/ 或 host 就是 relay）。 */
export function detectLinkKind(loc: LinkLocation, tunnelUrl = ''): LinkKind {
  if (relayDeviceFromPath(loc.pathname) !== '') return 'relay'
  const relayHost = hostOf(tunnelUrl)
  if (relayHost && relayHost === loc.host.toLowerCase()) return 'relay'
  return 'direct'
}

/** 内网/本机地址判定。只用来加一句提示，不影响链路结论。 */
export function isLanHost(host: string): boolean {
  let h = (host || '').toLowerCase().trim()
  // IPv6 是带方括号的 host：`[::1]:5173` → `::1`。按冒号裸切会把 `[` 当成主机名。
  if (h.startsWith('[')) {
    const end = h.indexOf(']')
    h = end > 0 ? h.slice(1, end) : h
  } else {
    h = h.split(':')[0]
  }
  if (h === 'localhost' || h === '::1') return true
  if (/^127\./.test(h)) return true
  if (/^10\./.test(h)) return true
  if (/^192\.168\./.test(h)) return true
  const m = /^172\.(\d+)\./.exec(h)
  if (m) {
    const second = Number(m[1])
    return second >= 16 && second <= 31
  }
  // .local 是 mDNS：局域网专用。
  return h.endsWith('.local')
}

/**
 * 把 /v1/remote/tunnel 的响应收敛成固定形状。
 *
 * 后端在未配置时会回 `{"configured":false}`，其余字段全缺；旧二进制则压根
 * 没这条路由（503）。缺失字段一律补默认值，界面用 `configured` 一个字段
 * 决定说什么，不再散落 `?? false`。
 */
export function normalizeTunnel(raw: unknown): TunnelState {
  const o = (raw || {}) as Record<string, unknown>
  const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)
  return {
    configured: !!o.configured,
    connected: !!o.connected,
    url: typeof o.url === 'string' ? o.url : '',
    deviceId: typeof o.deviceId === 'string' ? o.deviceId : '',
    streams: num(o.streams),
    reconnects: num(o.reconnects),
    lastError: typeof o.lastError === 'string' ? o.lastError : '',
  }
}

/**
 * 综合出「这一屏顶部该显示什么」。
 *
 * tunnel 传 null 表示接口还没返回或不可用（旧二进制 503）：这时只能说
 * 「未知」，不能断言「未配置」——那是两件事，混淆会让用户去改一个没问题的配置。
 */
export function describeLink(loc: LinkLocation, tunnel: TunnelState | null): LinkReport {
  const kind = detectLinkKind(loc, tunnel?.url)
  const device = relayDeviceFromPath(loc.pathname) || tunnel?.deviceId || ''

  if (!tunnel) {
    return {
      kind,
      tone: 'off',
      label: '链路未知',
      summary:
        kind === 'relay'
          ? '页面来自中继，但读不到隧道状态（控制面可能未装配中继模块）'
          : '读不到隧道状态（接口不可达或控制面未装配中继模块）',
      details: [],
    }
  }

  if (!tunnel.configured) {
    return {
      kind,
      tone: 'warn',
      label: kind === 'relay' ? '经中继' : '直连',
      summary: NO_RELAY_HINT,
      details: [],
      hint: '部署 astra-relay 后，手机在任意网络都能访问；见 文档 → 远程桌面 → 外网怎么连',
    }
  }

  if (!tunnel.connected) {
    return {
      kind,
      tone: 'warn',
      label: '中继断开',
      summary: '已配置外网中继，但电脑与 relay 的连接没建立',
      details: tunnelDetails(tunnel, device),
      hint: tunnel.lastError
        ? `relay 侧提示：${tunnel.lastError}`
        : '先看电脑端启动日志里的「外网中继已启动」，再核对 token 与 relay 地址',
    }
  }

  if (kind === 'relay') {
    return {
      kind,
      tone: 'ok',
      label: '经中继',
      summary: '手机经公网中继访问这台电脑',
      details: tunnelDetails(tunnel, device),
    }
  }

  // 直连 + 中继可用：两条路都通，说清楚「你现在走的是近的那条」。
  const details = tunnelDetails(tunnel, device)
  if (isLanHost(loc.host)) details.push('同一局域网内直连，延迟最低')
  return {
    kind,
    tone: 'ok',
    label: '直连',
    summary: '手机与电脑直连（不经过 relay）',
    details,
  }
}

function tunnelDetails(tunnel: TunnelState, device: string): string[] {
  const out: string[] = []
  if (device) out.push(`设备 ${device}`)
  if (tunnel.url) out.push(tunnel.url.replace(/\/+$/, ''))
  if (tunnel.streams > 0) out.push(`在途 ${tunnel.streams} 条流`)
  if (tunnel.reconnects > 0) out.push(`已重连 ${tunnel.reconnects} 次`)
  return out
}

/** 手机该访问的地址。拿不到 relay 地址或设备 id 时返回空串。 */
export function relayAccessUrl(tunnel: TunnelState): string {
  if (!tunnel.configured || !tunnel.url || !tunnel.deviceId) return ''
  return `${tunnel.url.replace(/\/+$/, '')}/t/${encodeURIComponent(tunnel.deviceId)}/`
}

function hostOf(url: string): string {
  if (!url) return ''
  try {
    return new URL(url).host.toLowerCase()
  } catch {
    return ''
  }
}

// ── 画面传输链路（与「页面从哪来」是两件事）────────────────────
//
// 顶部要同时回答两个问题：
//   「画面是 P2P 还是绕中继？」→ 传输链路
//   「页面本身是不是经中继打开的？」→ detectLinkKind
// 前者看 WebRTC 协商结果，后者看 URL。别把两者混成一个标签。

export type Transport = 'negotiating' | 'rtc' | 'ws'

export type TransportLabel = {
  /** 给人看的链路名：WebRTC 直连 / 中继(WS)。 */
  label: string
  /** 对端语义：P2P（点对点）还是 relay（经中继）。 */
  peer: 'P2P' | 'relay'
  tone: 'ok' | 'wait' | 'off'
}

/**
 * 协商中也显示「中继(WS)」：此刻画面确实是从 WS 出来的，
 * 写「协商中」会让用户以为画面还没来。
 */
export function transportLabel(t: Transport): TransportLabel {
  if (t === 'rtc') return { label: 'WebRTC 直连', peer: 'P2P', tone: 'ok' }
  if (t === 'negotiating') return { label: '中继(WS)', peer: 'relay', tone: 'wait' }
  return { label: '中继(WS)', peer: 'relay', tone: 'off' }
}
