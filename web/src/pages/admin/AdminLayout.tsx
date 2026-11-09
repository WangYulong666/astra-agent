// 管理后台外壳：顶栏 + 左侧 tab。
//
// tab 用 hash 路由而不是本地 state，理由只有一个——地址可分享。
// 「把这个用户的用量页发给我」是运维的日常需求。

import { TopBar } from '../../components/TopBar'
import { useAuth } from '../../auth'
import { Users } from './Users'
import { Keys } from './Keys'
import { Usage } from './Usage'
import { Audit } from './Audit'
import { Sandboxes } from './Sandboxes'
import { Lifecycle } from './Lifecycle'
import { Tenants } from './Tenants'
import { Settings } from './Settings'
import { Observability } from './Observability'
import { Memory } from './Memory'
import { Skills } from './Skills'
import { ActivityLedger } from './ActivityLedger'
import { Capabilities } from './Capabilities'
import { Insights } from './Insights'
import { Docs } from './Docs'
import { Remote } from './Remote'

type Tab = { path: string; label: string; ownerOnly?: boolean }

const TABS: Tab[] = [
  { path: '/admin/users', label: '用户' },
  { path: '/admin/keys', label: 'API Key' },
  { path: '/admin/usage', label: '用量' },
  { path: '/admin/audit', label: '审计' },
  { path: '/admin/sandboxes', label: '沙箱' },
  { path: '/admin/lifecycle', label: '回收' },
  // 平台扩展的三个面板排在「运行时治理」之后、「租户/设置」之前：
  // 它们看的是能力开没开，与上面的沙箱/回收同属运行时，而租户与设置是全局配置，
  // 放最后天然是「收尾」的位置。
  { path: '/admin/observability', label: '可观测' },
  { path: '/admin/memory', label: '记忆' },
  { path: '/admin/skills', label: '能力包' },
  // 自闭环两个面板放在「行动账本」附近：都看「平台跑完一轮留下了什么」。
  { path: '/admin/capabilities', label: '能力注册表' },
  // 文档紧跟能力注册表：一个是「平台有什么能力」，一个是「怎么用、为什么这么设计」，
  // 运维排障时通常在两者之间来回跳。
  { path: '/admin/docs', label: '文档' },
  // 远程桌面紧跟文档：都不是「平台数据」，而是「这台机器本身的状态」——
  // 排障时经常是从文档查到「要开哪个权限」，接着就来这一页看灯。
  { path: '/admin/remote', label: '远程桌面' },
  { path: '/admin/insights', label: '失败诊断' },
  { path: '/admin/activity', label: '行动账本' },
  // 租户管理只有 owner 有权限（admin 的权限表里没有 tenant:manage），
  // 给 admin 显示一个必然 403 的入口没有意义。
  { path: '/admin/tenants', label: '租户', ownerOnly: true },
  { path: '/admin/settings', label: '设置' },
]

export function AdminLayout({ path }: { path: string }) {
  const { isOwner } = useAuth()

  const page = (() => {
    switch (path) {
      case '/admin/users':
        return <Users />
      case '/admin/keys':
        return <Keys />
      case '/admin/usage':
        return <Usage />
      case '/admin/audit':
        return <Audit />
      case '/admin/sandboxes':
        return <Sandboxes />
      case '/admin/lifecycle':
        return <Lifecycle />
      case '/admin/observability':
        return <Observability />
      case '/admin/memory':
        return <Memory />
      case '/admin/skills':
        return <Skills />
      case '/admin/capabilities':
        return <Capabilities />
      case '/admin/docs':
        return <Docs />
      case '/admin/remote':
        return <Remote />
      case '/admin/insights':
        return <Insights />
      case '/admin/activity':
        return <ActivityLedger />
      case '/admin/tenants':
        return isOwner ? <Tenants /> : <Denied />
      case '/admin/settings':
        return <Settings />
      default:
        return <Users />
    }
  })()

  return (
    <div className="admin-app">
      <TopBar />
      <div className="admin-body">
        <aside className="admin-side">
          {TABS.filter((t) => !t.ownerOnly || isOwner).map((t) => (
            <a
              key={t.path}
              className={`admin-tab ${path === t.path ? 'on' : ''}`}
              href={`#${t.path}`}
            >
              {t.label}
            </a>
          ))}
        </aside>
        <main className="admin-main">{page}</main>
      </div>
    </div>
  )
}

function Denied() {
  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">租户</div>
          <div className="page-sub">只有租户所有者（owner）可以查看和修改这一页。</div>
        </div>
      </div>
    </div>
  )
}
