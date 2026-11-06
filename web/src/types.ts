// 与 Go 侧 internal/store/types.go 一一对应。
// 字段名刻意保持 camelCase 一致，不在中间做映射层——多一层映射就多一处会漂移的地方。

export type Session = {
  id: string
  sandboxId: string
  agentSessionId: string
  title?: string
  /** 非空表示这是父会话派生的子 agent（多 agent 协作）。 */
  parentSessionId?: string
  cwd: string
  status: string
  createdAt: string
  lastActiveAt: string
}

export type Run = {
  id: string
  sessionId: string
  /** 非空表示由父 run 派生。 */
  parentRunId?: string
  status: RunStatus
  input: string
  stopReason?: string
  error?: string
  nextSeq: number
  createdAt: string
  startedAt?: string
  endedAt?: string
}

export type RunStatus =
  | 'queued'
  | 'running'
  | 'waiting_approval'
  | 'completed'
  | 'failed'
  | 'cancelled'

export type RunEvent = {
  seq: number
  type: string
  payload: any
  createdAt: string
}

export type PendingApproval = {
  runId: string
  toolCallId: string
  title: string
  kind: string
  createdAt: string
  expiresAt: string
}

export type FileEntry = {
  name: string
  path: string
  isDir: boolean
  size: number
  modTime: string
}

export type FileContent = {
  path: string
  content: string
  size: number
  truncated: boolean
  binary: boolean
}

// ── 治理层（字段与 internal/store/admin.go 的 json tag 一一对应）──────
//
// 一个坑要注意：Go 的 `omitempty` 对 time.Time 不生效（结构体零值不算 empty），
// 所以「从未登录」的用户会拿到 "0001-01-01T00:00:00Z" 而不是缺字段。
// 展示层统一走 format.ts 的 fmtTime，别直接 new Date()。

export type Role = 'owner' | 'admin' | 'member'

export type User = {
  id: string
  tenantId: string
  email: string
  displayName: string
  role: Role
  status: 'active' | 'suspended'
  createdAt: string
  lastLoginAt?: string
}

export type Tenant = {
  id: string
  name: string
  slug: string
  status: 'active' | 'suspended'
  monthlyTokenLimit: number
  createdAt: string
}

export type APIKey = {
  id: string
  tenantId: string
  userId: string
  name: string
  prefix: string
  createdAt: string
  lastUsedAt?: string
  expiresAt?: string
  revokedAt?: string
}

export type Usage = {
  id: string
  tenantId: string
  userId?: string
  sessionId?: string
  runId?: string
  provider: string
  model: string
  inputTokens: number
  outputTokens: number
  costMicroUsd: number
  source: string
  createdAt: string
}

export type UsageSum = {
  inputTokens: number
  outputTokens: number
  costMicroUsd: number
  calls: number
}

export type UsageReport = {
  items: Usage[]
  sum: UsageSum
}

export type AuditLog = {
  id: string
  tenantId: string
  userId?: string
  actorEmail?: string
  action: string
  targetType: string
  targetId?: string
  detail?: unknown
  ip?: string
  createdAt: string
}

export type SandboxView = {
  id: string
  provider: string
  state: string
  workspace?: string
  pid?: number
  createdAt: string
  sessionId?: string
  sessionTitle?: string
  tenantId?: string
  userId?: string
}

export type LoginResponse = {
  token: string
  expiresAt: string
  user: User
  tenant: Tenant
}

export type BootstrapResponse = {
  tenant: Tenant
  user: User
}

export type MeResponse = {
  principal?: {
    userId: string
    tenantId: string
    email: string
    role: Role
    via: string
  }
  user?: User
}

export const ROLE_LABEL: Record<string, string> = {
  owner: '所有者',
  admin: '管理员',
  member: '成员',
}

export const STATUS_TEXT: Record<string, string> = {
  active: '启用',
  suspended: '停用',
}

export const TERMINAL: RunStatus[] = ['completed', 'failed', 'cancelled']

export function isTerminal(s: string): boolean {
  return s === 'completed' || s === 'failed' || s === 'cancelled'
}

export const STATUS_LABEL: Record<string, string> = {
  queued: '排队中',
  running: '执行中',
  waiting_approval: '等待审批',
  completed: '已完成',
  failed: '失败',
  cancelled: '已停止',
}

// 沙箱回收策略。时长的两种形态都由后端给：秒数用于比较，可读串用于展示。
// 前端不再自己换算——换算逻辑分散在两处时，界面显示的和实际执行的不一致，
// 那比不显示更糟。
export type LifecyclePolicy = {
  sweepIntervalSeconds: number
  idleTimeoutSeconds: number
  maxAgeSeconds: number
  maxPerTenant: number
  sweepInterval: string
  idleTimeout: string
  maxAge: string
  // enabled 为 false 表示阈值全为 0：后台循环不启动，手动清理也不会销毁任何东西。
  enabled: boolean
}

export type LifecycleReport = {
  scanned: number
  destroyed: number
  orphaned: number
  expired: number
  idle: number
  kept: number
  errors: string[]
  startedAt: string
  finishedAt: string
}

export type LifecycleView = {
  policy: LifecyclePolicy
  // 尚未扫描过时后端不给这个字段（不是给一个全零报告）。
  lastSweep?: LifecycleReport
}
