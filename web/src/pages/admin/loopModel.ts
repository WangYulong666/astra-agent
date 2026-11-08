// 自闭环三个页面（能力注册表 / 能力包 / 失败诊断）共用的纯展示逻辑。
//
// 单独拆成 .ts（不含 JSX）：vitest 跑在 node 环境、不引 React 插件，
// 这些映射 / 计数 / 筛选 / 503 与空态判断才能在单测里直接 import。
// 页面本身只负责取数与渲染。

import type { Capability, CapabilityKind, Skill } from '../../api'

// ── 映射 ─────────────────────────────────────────────────────

const KIND_LABEL: Record<string, string> = {
  api: 'API',
  cli: 'CLI',
  skill: 'Skill',
}

/** kind → 中文表头标签；未知值原样返回，不冒充某一类。 */
export function kindLabel(kind: string): string {
  return KIND_LABEL[kind] ?? kind
}

export type Tone = 'ready' | 'ok' | 'warn' | 'failed' | 'dim'

/** 技能状态 → 徽标文案/色调：ready 绿、unavailable 黄。 */
export function skillStatus(s: Skill): { label: string; tone: Tone } {
  return s.status === 'ready'
    ? { label: 'ready', tone: 'ready' }
    : { label: 'unavailable', tone: 'warn' }
}

/** 诊断状态 → 徽标文案/色调。未知状态退到中性，不标红冒充错误。 */
export function insightStatus(status: string): { label: string; tone: Tone } {
  switch ((status || '').toLowerCase()) {
    case 'failed':
      return { label: '失败', tone: 'failed' }
    case 'cancelled':
      return { label: '已停止', tone: 'dim' }
    case 'completed':
      return { label: '已完成', tone: 'ok' }
    default:
      return { label: status || '—', tone: 'dim' }
  }
}

// ── 计数 ─────────────────────────────────────────────────────

export type CapabilityCounts = {
  api: number
  cli: number
  skill: number
  enabled: number
  disabled: number
}

/** 三个 kind 各多少 + 启用/未启用各多少。基于全量清单，不受筛选影响。 */
export function countCapabilities(items: Capability[] | null | undefined): CapabilityCounts {
  const c: CapabilityCounts = { api: 0, cli: 0, skill: 0, enabled: 0, disabled: 0 }
  for (const it of items || []) {
    if (it.kind === 'api') c.api++
    else if (it.kind === 'cli') c.cli++
    else if (it.kind === 'skill') c.skill++
    if (it.enabled) c.enabled++
    else c.disabled++
  }
  return c
}

// ── 筛选（前端过滤，不额外打接口）─────────────────────────────

export type CapabilityFilter = {
  kind: CapabilityKind | 'all'
  onlyEnabled: boolean
  query: string
}

export function filterCapabilities(
  items: Capability[] | null | undefined,
  f: CapabilityFilter,
): Capability[] {
  const q = (f.query || '').trim().toLowerCase()
  return (items || []).filter((it) => {
    if (f.kind !== 'all' && it.kind !== f.kind) return false
    if (f.onlyEnabled && !it.enabled) return false
    if (q) {
      const hay = [
        it.name,
        it.description,
        it.kind,
        it.source || '',
        it.endpoint || '',
        ...(it.requires || []),
      ]
        .join(' ')
        .toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  })
}

// ── 503 / 空态 ───────────────────────────────────────────────

/**
 * 能力「未启用」判定。契约里能力未启用返回 503；此外兼容旧二进制的等价情形：
 * 路由未注册 → 404、网关/代理 → 5xx、控制面没连上 → 0。
 * 这些都说明「拿不到清单」，而不是「清单是空的」——一律走未启用引导，不画空图表。
 * 其它 4xx（如 401/403）是真实错误，交回页面按错误处理。
 */
export function isUnavailable(e: unknown): boolean {
  const s = (e as { status?: number } | null | undefined)?.status
  if (typeof s !== 'number') return false
  return s === 503 || s === 404 || s === 0 || (s >= 501 && s <= 599)
}

/** 错误单元格：展示截断，完整文本走 title（调用方挂 title）。 */
export function truncate(s: string | undefined | null, n = 48): string {
  if (!s) return ''
  const t = String(s).replace(/\s+/g, ' ').trim()
  return t.length > n ? `${t.slice(0, n)}…` : t
}

export type LoopPage = 'skills' | 'capabilities' | 'insights'

export function emptyText(page: LoopPage, filtered: boolean): string {
  if (filtered) {
    if (page === 'insights') return '没有匹配该 session_id 的失败诊断'
    if (page === 'capabilities') return '没有符合筛选条件的能力'
    return '没有符合筛选条件的技能'
  }
  if (page === 'insights') return '还没有失败诊断：跑一条失败的 run 之后回来看'
  if (page === 'capabilities') return '能力注册表里还没有条目'
  return '这个仓库还没有能力包（skills/ 目录为空）'
}

export type GuideRow = { k: string; v: string }
export type GuideSpec = { title: string; intro: string; rows: GuideRow[]; note: string }

/** 503 未启用时的开箱指引，逐条列出要配什么，而不是写一句「去开能力」。 */
export const GUIDES: Record<LoopPage, GuideSpec> = {
  skills: {
    title: '技能注册表未启用',
    intro:
      'GET /v1/skills 返回 503：技能注册表（skillhub）当前未装配或未启用，真实清单拿不到。',
    rows: [
      {
        k: 'SKILL_DIR',
        v: '默认 ~/.astra/skills。静态 skill 与 distill accept 产出的动态 skill 都放这里，启动时由仓库 skills/ 同步进来。',
      },
      {
        k: '/v1/meta 开关',
        v: '确认 skills 能力开关为 on；关闭时接口按未启用返回 503。',
      },
      {
        k: 'astra-cli 可执行',
        v: 'requires 校验依赖它；不可执行的 skill 会标 unavailable，不下发给 agent。',
      },
    ],
    note: '配好后重启控制面即可，平台其余功能不受影响。distill accept 后新增的 skill 会直接出现在这里，不再需要改前端清单。',
  },
  capabilities: {
    title: '能力注册表未启用',
    intro:
      'GET /v1/capabilities 返回 503：能力注册表（internal/capability）当前未装配或未启用，清单拿不到。',
    rows: [
      {
        k: '能力开关',
        v: '确认 /v1/meta 里 capability 相关开关为 on；关闭时接口按未启用返回 503。',
      },
      {
        k: 'astra-cli / admin-cli',
        v: 'CLI 类能力来自两个命令表的结构化清单（CommandSpec）；命令表没接线会让 CLI 条目缺失。',
      },
      {
        k: 'SKILL_DIR',
        v: 'Skill 类能力来自 skills 目录扫描；目录不可读时 skill 条目会标 unavailable。',
      },
    ],
    note: '配好后重启控制面即可。第一层只回 name/kind/description/enabled，打开「显示详情」才请求 ?detail=full 拉取 endpoint / requiredEnv / requires / unavailable。',
  },
  insights: {
    title: '失败诊断未启用',
    intro:
      'GET /v1/insights 返回 503：诊断存储（internal/insight）当前未装配或未启用，读不到结构化诊断。',
    rows: [
      {
        k: 'INSIGHT_REPLAY',
        v: '回灌开关：开时把最近一条未消诊断作为下一轮 run 的 prompt 前缀；设为 false 可关闭回灌但不影响记录。',
      },
      {
        k: 'insights.json',
        v: '诊断落盘在数据目录，原子写、上限 500 条；文件不可写时接口按未启用处理。',
      },
      {
        k: 'run 终态订阅',
        v: '只有 status=failed/cancelled 的 run 会生成诊断；成功 run 不会产生条目。',
      },
    ],
    note: '配好后重启控制面即可。suggestion 是规则式建议（不调 LLM），明确不会自动改平台代码或自动重试。',
  },
}
