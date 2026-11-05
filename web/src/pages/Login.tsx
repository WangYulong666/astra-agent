// 登录 / 首次初始化。
//
// 只在服务端明确要求认证时才会出现（判断逻辑在 auth.probeAuth），
// 所以这个页面不需要再考虑「兼容模式」——那种情况下它根本不会渲染。

import { useState } from 'react'
import { ApiError, authApi } from '../api'
import { useAuth } from '../auth'
import { Field, Notice } from '../components/ui'

type Mode = 'login' | 'bootstrap'

/** 把后端的失败翻译成用户能行动的话。分不清原因就等于让用户瞎试。 */
function describeLoginError(e: unknown): string {
  if (!(e instanceof ApiError)) return (e as Error)?.message || '登录失败'
  switch (e.status) {
    case 0:
      return '网络不通：连不上控制面。确认服务已启动、地址和端口正确。'
    case 400:
      return e.message // 缺字段之类，后端文案本身就是具体原因
    case 401:
      // 后端刻意不区分「租户不存在 / 用户不存在 / 密码错」（防账号枚举），
      // 所以这里也不能假装知道是哪一个，只能让用户逐项核对。
      return '租户、邮箱或密码不正确。为避免泄露账号是否存在，后端不会说明是哪一项。'
    case 403:
      return '租户已停用，请联系平台管理员开通。'
    case 501:
      return '服务端未启用认证（兼容模式）。刷新页面即可直接进入工作台。'
    default:
      return e.message
  }
}

function describeBootstrapError(e: unknown): string {
  if (!(e instanceof ApiError)) return (e as Error)?.message || '初始化失败'
  switch (e.status) {
    case 0:
      return '网络不通：连不上控制面。确认服务已启动、地址和端口正确。'
    case 409:
      return '系统已经初始化过了（只允许一次），请切回「登录」。'
    case 501:
      return '服务端没有接入治理存储，无法初始化。需要用 -store sqlite 或 postgres 启动。'
    default:
      return e.message
  }
}

export function Login() {
  const { login, probeError } = useAuth()
  const [mode, setMode] = useState<Mode>('login')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [hint, setHint] = useState('')

  // 登录
  const [tenant, setTenant] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  // 初始化
  const [tenantName, setTenantName] = useState('')
  const [slug, setSlug] = useState('')
  const [ownerEmail, setOwnerEmail] = useState('')
  const [ownerPassword, setOwnerPassword] = useState('')

  const switchMode = (m: Mode) => {
    if (busy) return
    setMode(m)
    setErr('')
    setHint('')
  }

  const doLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    setHint('')
    setBusy(true)
    try {
      await login({ tenant: tenant.trim(), email: email.trim(), password })
    } catch (e2) {
      setErr(describeLoginError(e2))
    } finally {
      setBusy(false)
    }
  }

  const doBootstrap = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    setHint('')
    setBusy(true)
    try {
      // bootstrap 只建租户和 owner，不回 token（见 auth_handlers.go 的 201 响应体），
      // 所以这里必须紧接着登一次。分成两步而不是让用户再手输一遍，
      // 是因为他刚刚输入的凭据就是有效的。
      await authApi.bootstrap({
        tenantName: tenantName.trim(),
        slug: slug.trim(),
        email: ownerEmail.trim(),
        password: ownerPassword,
      })
      setHint('初始化完成，正在登录…')
      await login({ tenant: slug.trim(), email: ownerEmail.trim(), password: ownerPassword })
    } catch (e2) {
      setErr(describeBootstrapError(e2))
      setHint('')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <div className="login-card">
        <div className="login-brand">
          <span className="logo" />
          <div>
            <div className="login-title">ASTRA</div>
            <div className="login-sub">Agent 工作台</div>
          </div>
        </div>

        <div className="seg">
          <button
            className={`seg-item ${mode === 'login' ? 'on' : ''}`}
            onClick={() => switchMode('login')}
            type="button"
          >
            登录
          </button>
          <button
            className={`seg-item ${mode === 'bootstrap' ? 'on' : ''}`}
            onClick={() => switchMode('bootstrap')}
            type="button"
          >
            首次初始化
          </button>
        </div>

        {probeError && (
          <Notice kind="warn">
            {probeError}
            <div className="notice-sub">服务恢复后刷新页面即可继续。</div>
          </Notice>
        )}

        {mode === 'login' ? (
          <form onSubmit={doLogin} className="login-form">
            <Field label="租户" hint="初始化时填的 slug，不是显示名">
              <input
                className="input"
                value={tenant}
                onChange={(e) => setTenant(e.target.value)}
                autoComplete="organization"
                placeholder="acme"
                required
              />
            </Field>
            <Field label="邮箱">
              <input
                className="input"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                autoComplete="username"
                placeholder="you@example.com"
                required
              />
            </Field>
            <Field label="密码">
              <input
                className="input"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                required
              />
            </Field>

            {err && <Notice kind="error">{err}</Notice>}
            {hint && <Notice kind="info">{hint}</Notice>}

            <button className="btn-primary" type="submit" disabled={busy}>
              {busy && <span className="spinner" />}
              {busy ? '正在登录…' : '登录'}
            </button>
          </form>
        ) : (
          <form onSubmit={doBootstrap} className="login-form">
            <Field label="租户名称" hint="显示用，可以随时改">
              <input
                className="input"
                value={tenantName}
                onChange={(e) => setTenantName(e.target.value)}
                placeholder="Acme 研发部"
                required
              />
            </Field>
            <Field label="租户标识" hint="登录时用它。小写字母、数字、连字符，3~32 位，创建后不可改">
              <input
                className="input"
                value={slug}
                onChange={(e) => setSlug(e.target.value.toLowerCase())}
                placeholder="acme"
                required
              />
            </Field>
            <Field label="所有者邮箱">
              <input
                className="input"
                type="email"
                value={ownerEmail}
                onChange={(e) => setOwnerEmail(e.target.value)}
                autoComplete="username"
                placeholder="you@example.com"
                required
              />
            </Field>
            <Field label="密码" hint="至少 8 位。owner 拥有租户的全部权限，请用强口令">
              <input
                className="input"
                type="password"
                value={ownerPassword}
                onChange={(e) => setOwnerPassword(e.target.value)}
                autoComplete="new-password"
                required
              />
            </Field>

            {err && <Notice kind="error">{err}</Notice>}
            {hint && <Notice kind="info">{hint}</Notice>}

            <button className="btn-primary" type="submit" disabled={busy}>
              {busy && <span className="spinner" />}
              {busy ? '正在初始化…' : '创建租户并登录'}
            </button>
            <p className="login-note">
              只有在系统里还没有任何租户时才能初始化。已经启用过的话，请让管理员在管理后台给你建账号。
            </p>
          </form>
        )}
      </div>
    </div>
  )
}
