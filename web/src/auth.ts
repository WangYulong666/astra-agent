// 认证状态。
//
// 为什么不用 Context：整个应用只有「顶栏」和「门卫」两处读它，而门卫本身就
// 是包裹一切的组件——用 Context 只是把同一个全局变量换个写法，还多一层
// Provider 的嵌套顺序要维护。这里用一个带订阅的模块级对象，任何组件都能
// 通过 useAuth() 读到，刷新粒度交给 React。
//
// 状态存 localStorage 的理由：JWT 是 24h 的，刷新页面不该重新登录。
// 存 token 而不是 cookie 是后端的选择（见设计文档 D1），前端跟着走。

import { useEffect, useState } from 'react'
import { ApiError, api, authApi, configureAuth } from './api'
import type { Tenant, User } from './types'

const TOKEN_KEY = 'astra_token'
const USER_KEY = 'astra_user'
const TENANT_KEY = 'astra_tenant'

export type AuthState = {
  token: string
  user: User | null
  tenant: Tenant | null
  /** 服务端是否强制登录。探测前按 true 处理：宁可多要一次登录，也不要把未认证的界面露出来。 */
  requireAuth: boolean
  /** 是否已经问过服务端。false 时界面显示启动占位，不能立刻判定「未登录」。 */
  ready: boolean
  /** 探测失败的原因（网络不通等）。登录页要把它显示出来，否则用户只看到空表单。 */
  probeError: string
}

function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    // 隐私模式下 localStorage 读写都会抛，这时退化成「不记住登录」即可
    return null
  }
}

function writeStorage(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key)
    else localStorage.setItem(key, value)
  } catch {
    /* 同上：存不住不影响本次会话 */
  }
}

function readJSON<T>(key: string): T | null {
  const raw = readStorage(key)
  if (!raw) return null
  try {
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

let state: AuthState = {
  token: readStorage(TOKEN_KEY) || '',
  user: readJSON<User>(USER_KEY),
  tenant: readJSON<Tenant>(TENANT_KEY),
  requireAuth: true,
  ready: false,
  probeError: '',
}

const listeners = new Set<() => void>()

function set(patch: Partial<AuthState>) {
  state = { ...state, ...patch }
  for (const l of listeners) l()
}

function saveSession(token: string, user: User, tenant: Tenant | null) {
  writeStorage(TOKEN_KEY, token)
  writeStorage(USER_KEY, JSON.stringify(user))
  writeStorage(TENANT_KEY, tenant ? JSON.stringify(tenant) : null)
  set({ token, user, tenant, ready: true, probeError: '' })
}

function clearSession() {
  writeStorage(TOKEN_KEY, null)
  writeStorage(USER_KEY, null)
  writeStorage(TENANT_KEY, null)
  set({ token: '', user: null, tenant: null })
}

// 防止 401 风暴：首屏会并发发好几个请求，一个过期 token 会让它们同时失败。
// 这个闸门顺带挡住「登录失败 → 跳登录页 → 登录页自己又收到 401 → 再跳一次」
// 这种自己踢自己的循环。时间窗只是兜底，主保护是 api 层的 skipAuthRedirect。
let redirecting = false

function handleUnauthorized() {
  if (redirecting) return
  // 兼容模式下没有「会话过期」这回事。真收到 401 说明手里的旧 token 对某个
  // 收紧过的接口无效——清掉它继续用即可，绝不能因此把本地无认证模式
  // 挡到登录页外面去（那会让 make serve 起来的环境突然用不了）。
  if (!state.requireAuth) {
    clearSession()
    return
  }
  // 本来就没有身份（比如正停在登录页），不需要再做任何事。
  if (!state.user) return
  redirecting = true
  clearSession()
  if (window.location.hash.startsWith('#/admin')) {
    // 用 replace：被踢出登录后按浏览器回退键不该回到上一个空壳页面。
    window.location.replace('#/')
  }
  window.setTimeout(() => {
    redirecting = false
  }, 1000)
}

// 单向注册：api.ts 只认这两个回调，不认识 auth 模块本身。
configureAuth({ getToken: () => state.token, onUnauthorized: handleUnauthorized })

/**
 * 启动探测：先问 /v1/meta 服务端要不要认证。
 *
 * 三种结果对应三种界面，必须分开处理：
 *   200 + requireAuth !== "true" → 兼容模式（本地无认证），直接进工作台
 *   200 + requireAuth === "true" → 带上的 token 有效，恢复身份即可
 *   401                          → 需要登录
 * 不做这一步的话，本地无认证模式会被登录页挡在门外——那是项目明确要保住的用法。
 */
export async function probeAuth(): Promise<void> {
  try {
    // meta 的 401 不触发跳转（api.meta 传了 skipAuthRedirect），
    // 探测本身就是靠这个 401 来判断「需要登录」的。
    const meta = await api.meta()
    if (meta.requireAuth !== 'true') {
      // 兼容模式：后端放行一切。已有的 token 保留不清——服务端没在使用它。
      set({ requireAuth: false, ready: true, probeError: '' })
      return
    }
    if (!state.token) {
      set({ requireAuth: true, ready: true, probeError: '' })
      return
    }
    // 有 token 且服务端要求认证：meta 能 200 说明 token 有效。
    // 再拉一次 me 是为了拿最新角色（后端可能刚把你降权），顶栏按钮的
    // 可见性依赖它，不能拿 localStorage 里的旧值凑合。
    const me = await authApi.me()
    if (me.user) {
      writeStorage(USER_KEY, JSON.stringify(me.user))
      set({ user: me.user, requireAuth: true, ready: true, probeError: '' })
    } else {
      set({ requireAuth: true, ready: true, probeError: '' })
    }
  } catch (e) {
    const err = e as ApiError
    if (err.status === 401) {
      // token 过期或被吊销：清掉，回到登录页。
      clearSession()
      set({ requireAuth: true, ready: true, probeError: '' })
      return
    }
    // 网络不通之类：仍然进登录页，但把原因写清楚——否则用户对着一个
    // 输入密码也没用的表单反复试。
    set({ requireAuth: true, ready: true, probeError: err.message })
  }
}

export type LoginInput = { tenant: string; email: string; password: string }

async function login(input: LoginInput): Promise<void> {
  const res = await authApi.login(input)
  saveSession(res.token, res.user, res.tenant)
  set({ requireAuth: true })
}

async function logout(): Promise<void> {
  try {
    await authApi.logout()
  } catch {
    // 后端只记审计，失败也代表不了什么；本地该清还是要清，
    // 否则「点退出没反应」比「退出没记上审计」严重得多。
  }
  clearSession()
}

async function changePassword(oldPassword: string, newPassword: string): Promise<void> {
  await authApi.changePassword(oldPassword, newPassword)
}

/** 能进管理后台的角色。未知角色按无权处理（最小权限）。 */
export function canManage(role: string): boolean {
  return role === 'owner' || role === 'admin'
}

export function useAuth() {
  // 订阅模块级状态：useState 只用来触发重渲染，值仍从 state 读，
  // 避免把「真相」复制成两份。
  const [, force] = useState(0)
  useEffect(() => {
    const l = () => force((n) => n + 1)
    listeners.add(l)
    return () => {
      listeners.delete(l)
    }
  }, [])

  const role = state.user?.role ?? ''
  return {
    user: state.user,
    tenant: state.tenant,
    role,
    // 兼容模式下没有 user，但后端全放行：管理入口该显示出来，
    // 否则本地单机版就看不到任何治理功能。
    canAdmin: state.ready && (!state.requireAuth || canManage(role)),
    isOwner: state.ready && (!state.requireAuth || role === 'owner'),
    requireAuth: state.requireAuth,
    probeError: state.probeError,
    /** 还没问过服务端（启动占位）。 */
    loading: !state.ready,
    login,
    logout,
    changePassword,
  }
}
