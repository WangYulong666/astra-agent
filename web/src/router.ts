// 极简 hash 路由。
//
// 二十来行就够的东西不值得引 react-router（它的体积比整个应用的业务代码还大）。
// 只做两件事：把 hash 归一化成路由路径、把 hashchange 转成重渲染。

import { useEffect, useState } from 'react'

export const ROUTES = [
  '/',
  '/admin/users',
  '/admin/keys',
  '/admin/usage',
  '/admin/audit',
  '/admin/sandboxes',
  '/admin/lifecycle',
  // 平台扩展的三个面板：可观测 / 记忆 / 能力包
  '/admin/observability',
  '/admin/memory',
  '/admin/skills',
  // 用户视角行动账本；自闭环两个面板排在它附近
  '/admin/activity',
  '/admin/capabilities',
  '/admin/insights',
  '/admin/tenants',
  '/admin/settings',
] as const

export type RoutePath = (typeof ROUTES)[number]

function readHash(): string {
  // hash 里可能带查询串（例如 #/admin/usage?days=7），路由只认路径部分
  const raw = window.location.hash.replace(/^#/, '')
  const path = raw.split('?')[0] || '/'
  // 未知路径一律落到工作台：手输错一个字母不该得到白屏
  return (ROUTES as readonly string[]).includes(path) ? path : '/'
}

export function navigate(path: string) {
  if (readHash() === path) return
  window.location.hash = path
}

export function useRoute(): string {
  const [path, setPath] = useState(readHash)
  useEffect(() => {
    const onHash = () => setPath(readHash())
    window.addEventListener('hashchange', onHash)
    // 首屏没有 hash 时补一个，刷新后地址栏不会空着
    if (!window.location.hash) window.location.replace('#/')
    return () => window.removeEventListener('hashchange', onHash)
  }, [])
  return path
}
