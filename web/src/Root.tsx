import { useEffect, useState } from 'react'
import App from './App'
import { Login } from './pages/Login'
import { AdminLayout } from './pages/admin/AdminLayout'
import { probeAuth, useAuth } from './auth'
import { setupApi, type SetupState } from './api'
import { Setup } from './pages/Setup'
import { navigate, useRoute } from './router'

/**
 * 门卫。三件事：启动探测、决定要不要登录、按路由分发。
 *
 * 顺序很关键：先探测再判断，否则刷新页面时会先闪一下登录页再跳回工作台
 * （带着有效 token 的用户看到登录框，会以为登录状态丢了）。
 */
export function Root() {
  const path = useRoute()
  const { loading, requireAuth, user, canAdmin } = useAuth()

  useEffect(() => {
    void probeAuth()
  }, [])

  const wantsAdmin = path.startsWith('/admin')

  // 空系统优先展示向导而不是登录框：桌面端首启用户面对的必须是
  // 「创建账号」而不是「输入一个还不存在的账号」。
  // ⚠️ hooks 必须在所有条件 return 之前——之前放在 if(loading) 后面，
  // 首帧 loading=true 提前 return、下一帧再走 hook，直接触发 React #310。
  const [setupState, setSetupState] = useState<SetupState | null>(null)
  useEffect(() => {
    if (loading || requireAuth === false) return
    void setupApi
      .state()
      .then((s) => setSetupState(s))
      .catch(() => setSetupState(null))
  }, [loading, requireAuth])

  useEffect(() => {
    // member 手输地址进来：直接送回工作台。不用报错页——那不是他的错，
    // 而且后端的 403 会让他以为系统出问题了。
    if (!loading && wantsAdmin && !canAdmin) navigate('/')
  }, [loading, wantsAdmin, canAdmin])

  if (loading) {
    return (
      <div className="boot">
        <span className="spinner" />
        <span>正在连接控制面…</span>
      </div>
    )
  }

  if (setupState?.needsSetup) return <Setup />

  // 只有服务端明确要求认证、且手上没有身份时才拦。兼容模式（requireAuth=false）
  // 永远直接进工作台——这是「clone 下来就能用」那条路径。
  if (requireAuth && !user) return <Login />

  if (wantsAdmin && canAdmin) return <AdminLayout path={path} />
  return <App />
}
