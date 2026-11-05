import type {
  APIKey,
  AuditLog,
  BootstrapResponse,
  FileContent,
  FileEntry,
  LifecycleReport,
  LifecycleView,
  LoginResponse,
  MeResponse,
  PendingApproval,
  Run,
  RunEvent,
  SandboxView,
  Session,
  Tenant,
  UsageReport,
  User,
} from './types'

// 同源访问：生产是 Go 内嵌的静态资源，开发是 vite 的 proxy。
// 不写死 host 是刻意的——换个域名、挂到 APISIX 后面都不需要改前端。
const BASE = ''

/**
 * 带状态码的错误。
 *
 * 原来只 throw 一个字符串，多个页面的错误分支就只能靠匹配中文文案——
 * 后端改一个字的措辞，前端的分支就静默走错。状态码才是稳定的判断依据。
 */
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

// 认证信息的注入点。api.ts 不直接 import auth.ts：两个模块互相引用时，
// 顶层求值顺序会随打包方式变化（dev 与 build 不一样），那种问题只在特定
// 构建产物上复现。改成由 auth.ts 单向注册，依赖方向就固定了。
let getToken: () => string = () => ''
let onUnauthorized: () => void = () => {}

export function configureAuth(opts: {
  getToken: () => string
  onUnauthorized: () => void
}): void {
  getToken = opts.getToken
  onUnauthorized = opts.onUnauthorized
}

export type RequestOptions = {
  /**
   * 401 时不要当作「会话失效」。
   * 登录、改密码这两个接口用 401 表示「密码不对」，那是一次正常的业务失败，
   * 若按会话失效处理会把用户从登录页踢回登录页（看起来像点了没反应）。
   */
  skipAuthRedirect?: boolean
}

async function request<T>(
  path: string,
  init?: RequestInit,
  opts?: RequestOptions,
): Promise<T> {
  const token = getToken()
  let res: Response
  try {
    res = await fetch(BASE + path, {
      ...init,
      headers: {
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(init?.headers || {}),
      },
    })
  } catch {
    // fetch 只在「压根没连上」时抛错（DNS/拒绝连接/被墙），这里统一成 status 0，
    // 调用方才能把「网络不通」和「服务端拒绝」分开提示。
    throw new ApiError(0, '网络不通：无法连接控制面')
  }
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      /* 响应体不是 JSON 就用状态码 */
    }
    if (res.status === 401 && !opts?.skipAuthRedirect) onUnauthorized()
    throw new ApiError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export const api = {
  // meta 带 requireAuth：前端靠它决定「要不要显示登录页」。
  // 兼容模式下它无需凭据即可读，强制认证模式下无 token 会 401——
  // 这两种结果本身就是登录页的探测依据，所以这里的 401 不触发跳转。
  meta: () => request<Record<string, string>>('/v1/meta', undefined, { skipAuthRedirect: true }),

  listSessions: () => request<Session[]>('/v1/sessions'),
  createSession: (cwd?: string) =>
    request<Session>('/v1/sessions', {
      method: 'POST',
      body: JSON.stringify(cwd ? { cwd } : {}),
    }),
  renameSession: (id: string, title: string) =>
    request<Session>(`/v1/sessions/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ title }),
    }),
  deleteSession: (id: string) =>
    request<void>(`/v1/sessions/${id}`, { method: 'DELETE' }),

  listRuns: (sessionId: string) =>
    request<Run[]>(`/v1/sessions/${sessionId}/runs`),
  listEvents: (runId: string, afterSeq = 0) =>
    request<RunEvent[]>(`/v1/runs/${runId}/history?after_seq=${afterSeq}`),

  startRun: (sessionId: string, input: string) =>
    request<Run>(`/v1/sessions/${sessionId}/runs`, {
      method: 'POST',
      body: JSON.stringify({ input }),
    }),
  cancelRun: (runId: string) =>
    request<void>(`/v1/runs/${runId}/cancel`, { method: 'POST' }),

  listApprovals: (runId: string) =>
    request<PendingApproval[]>(`/v1/runs/${runId}/approvals`),
  resolveApproval: (runId: string, toolCallId: string, decision: 'allow' | 'deny') =>
    request<unknown>(
      `/v1/runs/${runId}/approvals/${encodeURIComponent(toolCallId)}`,
      { method: 'POST', body: JSON.stringify({ decision }) },
    ),

  listFiles: (sessionId: string, path = '') =>
    request<{ path: string; entries: FileEntry[] }>(
      `/v1/sessions/${sessionId}/files?path=${encodeURIComponent(path)}`,
    ),
  readFile: (sessionId: string, path: string) =>
    request<FileContent>(
      `/v1/sessions/${sessionId}/file?path=${encodeURIComponent(path)}`,
    ),
}

// ── 身份 ─────────────────────────────────────────────────────

export const authApi = {
  login: (body: { tenant: string; email: string; password: string }) =>
    request<LoginResponse>(
      '/v1/auth/login',
      { method: 'POST', body: JSON.stringify(body) },
      { skipAuthRedirect: true },
    ),

  // 首次初始化只建租户和 owner，**不返 token**（见 auth_handlers.go），
  // 所以调用方拿到 201 之后必须再 login 一次才能进工作台。
  bootstrap: (body: {
    tenantName: string
    slug: string
    email: string
    password: string
  }) =>
    request<BootstrapResponse>(
      '/v1/auth/bootstrap',
      { method: 'POST', body: JSON.stringify(body) },
      { skipAuthRedirect: true },
    ),

  me: () => request<MeResponse>('/v1/auth/me'),

  // 401 在这里表示「原密码不正确」，不是会话失效。
  changePassword: (oldPassword: string, newPassword: string) =>
    request<void>(
      '/v1/auth/password',
      { method: 'PATCH', body: JSON.stringify({ oldPassword, newPassword }) },
      { skipAuthRedirect: true },
    ),

  logout: () => request<void>('/v1/auth/logout', { method: 'POST' }),
}

// ── 管理面 ───────────────────────────────────────────────────


export const setupApi = {
  state: () =>
    request<SetupState>('/v1/setup/state', {}, { skipAuthRedirect: true }),

  complete: (body: { slug?: string; name?: string; email: string; password: string }) =>
    request<SetupCompleteResponse>(
      '/v1/setup/complete',
      { method: 'POST', body: JSON.stringify(body) },
      { skipAuthRedirect: true },
    ),

  llmSettings: () => request<LLMSettings>('/v1/admin/llm-settings'),

  saveLLMSettings: (body: { apiKey: string; baseUrl?: string; keepKey?: boolean; clear?: boolean }) =>
    request<{ hasKey: boolean; baseUrl: string }>(
      '/v1/admin/llm-settings',
      { method: 'POST', body: JSON.stringify(body) },
    ),

  testLLM: (body: { apiKey: string; baseUrl?: string }) =>
    request<{ ok: boolean; latencyMs?: number; error?: string }>(
      '/v1/admin/llm-settings/test',
      { method: 'POST', body: JSON.stringify(body) },
    ),
}

export type SetupState = {
  needsSetup: boolean
  agentName?: string
  approval?: string
  llmGateway: boolean
  llmConfigured: boolean
}

export type SetupCompleteResponse = {
  autoLogin: boolean
  token?: string
  tenant: Tenant
  user: User
}

export type LLMSettings = { gateway: boolean; hasKey?: boolean; baseUrl?: string }

export const adminApi = {
  listUsers: () => request<User[]>('/v1/admin/users'),
  createUser: (body: {
    email: string
    displayName?: string
    role?: string
    password?: string
  }) =>
    request<{ user: User; initialPassword?: string }>('/v1/admin/users', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  updateUser: (
    id: string,
    patch: { role?: string; status?: string; displayName?: string; password?: string },
  ) =>
    request<User>(`/v1/admin/users/${id}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),

  listKeys: () => request<APIKey[]>('/v1/admin/apikeys'),
  createKey: (body: { name: string; userId?: string; expiresInDays?: number }) =>
    request<{ key: APIKey; plaintext: string }>('/v1/admin/apikeys', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  revokeKey: (id: string) =>
    request<void>(`/v1/admin/apikeys/${id}`, { method: 'DELETE' }),

  // 全租户用量。member 无权访问这个接口（需要 usage:read:all）。
  usage: (from: string, to = '') =>
    request<UsageReport>(
      `/v1/admin/usage?from=${encodeURIComponent(from)}${to ? `&to=${encodeURIComponent(to)}` : ''}`,
    ),

  audit: (params: { action?: string; from?: string; to?: string; limit?: number }) => {
    const q = new URLSearchParams()
    if (params.action) q.set('action', params.action)
    if (params.from) q.set('from', params.from)
    if (params.to) q.set('to', params.to)
    if (params.limit) q.set('limit', String(params.limit))
    return request<AuditLog[]>(`/v1/admin/audit?${q.toString()}`)
  },

  listSandboxes: () => request<SandboxView[]>('/v1/sandboxes'),
  killSandbox: (id: string) =>
    request<void>(`/v1/sandboxes/${id}`, { method: 'DELETE' }),

  listTenants: () => request<Tenant[]>('/v1/admin/tenants'),
  updateTenant: (
    id: string,
    patch: { name?: string; status?: string; monthlyTokenLimit?: number },
  ) =>
    request<Tenant>(`/v1/admin/tenants/${id}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),

  // 沙箱回收：策略与上一轮报告在同一个接口里。
  // 分开成两个接口的话，运维看到「沙箱没被回收」时永远少看一半信息。
  lifecycle: () => request<LifecycleView>('/v1/admin/lifecycle'),
  sweepNow: () =>
    request<LifecycleReport>('/v1/admin/lifecycle/sweep', { method: 'POST' }),
}

// 事件流里所有会出现的事件名。EventSource 只把无名事件交给 onmessage，
// 所以服务端用了 `event: <type>` 之后必须逐个 addEventListener。
// 漏一个的结果是「这类事件静默丢失」——比报错更难发现。
const EVENT_TYPES = [
  'run_state',
  'message_delta',
  'thought_delta',
  'tool_call_start',
  'tool_call_update',
  'approval_req',
  'approval_result',
  'agent_log',
  'error',
]

export type StreamHandlers = {
  onEvent: (ev: RunEvent) => void
  /** 服务端主动断开让客户端续传（缓冲满了，丢的是通知不是数据）。 */
  onOverflow: (lastSeq: number) => void
  /** 连接建立/断开，仅用于顶部状态灯。 */
  onState: (s: 'live' | 'retry' | 'closed') => void
}

/**
 * 订阅一个 run 的事件流。
 *
 * afterSeq 之后的历史由服务端先回放，所以调用方不需要自己补历史——
 * 但 UI 侧仍然会先拉一次 REST 历史（首屏更快），这里传 lastSeq 避免重复。
 *
 * 两种实现共存是有原因的：EventSource 不能自定义请求头，认证打开后它会
 * 拿不到 Authorization，服务端直接 401。所以带 token 时改用 fetch 流式读取，
 * 解析逻辑与 SSE 一致；兼容模式下仍走 EventSource——那条路径已经验证过，
 * 不为了统一而冒改坏的风险。
 */
export function subscribeEvents(
  runId: string,
  afterSeq: number,
  h: StreamHandlers,
): () => void {
  return getToken()
    ? subscribeWithFetch(runId, afterSeq, h)
    : subscribeWithEventSource(runId, afterSeq, h)
}

function subscribeWithEventSource(
  runId: string,
  afterSeq: number,
  h: StreamHandlers,
): () => void {
  let closed = false
  let last = afterSeq
  let es: EventSource | null = null
  let retryTimer: number | undefined

  const open = () => {
    if (closed) return
    const url = `${BASE}/v1/runs/${runId}/events?after_seq=${last}`
    es = new EventSource(url)

    es.onopen = () => h.onState('live')
    es.onerror = () => {
      // EventSource 自带重连，但服务端在 run 结束后会主动关连接，
      // 那种情况下重连只会拿到「已结束」再被关掉，形成 1 秒一次的空转。
      // 由调用方在收到终态时 close，这里只在还活着时报告重试。
      if (!closed) h.onState('retry')
    }

    for (const type of EVENT_TYPES) {
      es.addEventListener(type, (e) => {
        const me = e as MessageEvent
        let payload: any = {}
        try {
          payload = JSON.parse(me.data)
        } catch {
          payload = { raw: me.data }
        }
        const seq = Number(me.lastEventId || 0) || last + 1
        last = seq
        h.onEvent({ seq, type, payload, createdAt: new Date().toISOString() })
      })
    }

    es.addEventListener('overflow', () => {
      es?.close()
      h.onOverflow(last)
      // 让调用方决定要不要重开；这里自己重开更简单，且不影响外部状态。
      retryTimer = window.setTimeout(open, 300)
    })
  }

  open()

  return () => {
    closed = true
    if (retryTimer) window.clearTimeout(retryTimer)
    es?.close()
    h.onState('closed')
  }
}

/**
 * 用 fetch 读 text/event-stream。
 *
 * 只实现服务端实际会发的那一小部分协议：`id:` / `event:` / `data:` + 空行分隔，
 * 以及以 `:` 开头的注释帧（心跳）。做通用 SSE 客户端没有意义。
 */
function subscribeWithFetch(
  runId: string,
  afterSeq: number,
  h: StreamHandlers,
): () => void {
  let closed = false
  let last = afterSeq
  let ctrl: AbortController | null = null
  let retryTimer: number | undefined

  const dispatch = (block: string) => {
    let type = ''
    let data = ''
    let id = 0
    for (const line of block.split('\n')) {
      if (!line || line.startsWith(':')) continue // 心跳/注释
      const idx = line.indexOf(':')
      const field = idx < 0 ? line : line.slice(0, idx)
      const value = idx < 0 ? '' : line.slice(idx + 1).replace(/^ /, '')
      if (field === 'event') type = value
      else if (field === 'data') data += (data ? '\n' : '') + value
      else if (field === 'id') id = Number(value) || 0
    }
    if (!type) return
    if (type === 'overflow') {
      h.onOverflow(last)
      return
    }
    let payload: any = {}
    try {
      payload = JSON.parse(data)
    } catch {
      payload = { raw: data }
    }
    if (id) last = id
    h.onEvent({ seq: id || last + 1, type, payload, createdAt: new Date().toISOString() })
  }

  const schedule = (delay: number) => {
    if (closed) return
    h.onState('retry')
    if (retryTimer) window.clearTimeout(retryTimer)
    retryTimer = window.setTimeout(() => void open(), delay)
  }

  const open = async () => {
    if (closed) return
    ctrl = new AbortController()
    try {
      const res = await fetch(`${BASE}/v1/runs/${runId}/events?after_seq=${last}`, {
        headers: { Authorization: `Bearer ${getToken()}` },
        signal: ctrl.signal,
      })
      if (res.status === 401) {
        onUnauthorized()
        return
      }
      if (!res.ok || !res.body) {
        schedule(1500)
        return
      }
      h.onState('live')
      const reader = res.body.getReader()
      const dec = new TextDecoder()
      let buf = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buf += dec.decode(value, { stream: true }).replace(/\r\n/g, '\n')
        let cut = buf.indexOf('\n\n')
        while (cut >= 0) {
          dispatch(buf.slice(0, cut))
          buf = buf.slice(cut + 2)
          cut = buf.indexOf('\n\n')
        }
      }
      // 服务端在 run 终结时会主动断流。调用方收到终态后会 close，
      // 这里若还没 close 说明是意外断开，短等后重连一次拿终态。
      schedule(800)
    } catch {
      if (!closed) schedule(1500)
    }
  }

  void open()

  return () => {
    closed = true
    if (retryTimer) window.clearTimeout(retryTimer)
    ctrl?.abort()
    h.onState('closed')
  }
}

// ── 平台扩展：可观测 / 向量记忆 ───────────────────────────────
//
// 这两块的共同点是「能力可能关着」：没配 Langfuse / Milvus 时接口照常存在，
// 只是回 enabled=false（或 503 + 一句可操作的原因）。前端不做能力探测的
// 第二套逻辑，一律以接口回的东西为准——否则前后端会各说一套。

/** GET /v1/admin/obs/config。secret key 留在服务端，这里只有 public key。 */
export type ObsConfig = {
  enabled: boolean
  endpoint: string
  dashboard_url: string
  public_key: string
}

/** Langfuse trace 摘要（服务端代查，字段见 internal/trace.Summary）。 */
export type ObsTrace = {
  id: string
  name?: string
  run_id?: string
  status?: string
  latency_ms?: number
  started_at?: string
  url?: string
  /** 输入摘要：服务端已截断，前端不再二次处理 */
  input?: string
}

export const obsApi = {
  config: () => request<ObsConfig>('/v1/admin/obs/config'),

  traces: (runId = '', limit = 20) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (runId) q.set('run_id', runId)
    return request<{ items: ObsTrace[]; limit: number }>(`/v1/admin/obs/traces?${q.toString()}`)
  },
}

/** GET /v1/memory/stats。未配置 Milvus 时 enabled=false、backend=disabled。 */
export type MemoryStats = {
  enabled: boolean
  backend: string
  collection: string
  count: number
  dim: number
  embedder: string
}

/**
 * 一条记忆。score 只有检索结果才有（写入响应里是 0，被 omitempty 吞掉），
 * 所以这里必须是可选——展示层用「—」兜底，不要显示 0.000 骗人。
 */
export type MemoryItem = {
  id: string
  text: string
  score?: number
  kind: string
  created_at: string
  session_id?: string
}

export const memoryApi = {
  stats: () => request<MemoryStats>('/v1/memory/stats'),

  // 服务端强制按调用者的 tenant_id + user_id 过滤，前端没有、也不该有
  // 「按 user 查」的参数——传了也不会生效。
  search: (q: string, k = 10) =>
    request<{ items: MemoryItem[]; query: string; k: number }>(
      `/v1/memory/search?q=${encodeURIComponent(q)}&k=${k}`,
    ),
}
