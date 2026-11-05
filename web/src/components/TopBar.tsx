// 顶栏。工作台和管理后台共用：两套界面的标题、导航、用户菜单应当完全一致，
// 各写一份的结果是「切到管理页像是换了个产品」。

import { useEffect, useRef, useState } from 'react'
import { ApiError } from '../api'
import { useAuth } from '../auth'
import { useRoute, navigate } from '../router'
import { ROLE_LABEL } from '../types'
import { Field, Modal, Notice, IconSearch } from './ui'
import { IconMenu } from './icons'

export function TopBar({
  chips,
  actions,
  onMenu,
  onSearch,
}: {
  /** 中间区的运行信息（provider / 权限 / agent），只有工作台用得上 */
  chips?: React.ReactNode
  /** 右侧业务操作（连接状态、文件面板开关） */
  actions?: React.ReactNode
  /**
   * 窄屏下打开会话抽屉。工作台传，管理后台不传——后台的导航是顶部横向
   * tab 条，再给一个汉堡按钮会让人以为点开是同一份东西。
   * 这个按钮默认 display:none，只在 <=760px 出现，宽屏布局不受影响。
   */
  onMenu?: () => void
  /**
   * 窄屏下的会话搜索入口（点开是整屏搜索层）。
   * 桌面不给这个按钮：那边有 ⌘K，顶栏再多一个放大镜只会挤掉 chips。
   * 按钮同样默认 display:none，只有 <=767 出现。
   */
  onSearch?: () => void
}) {
  const path = useRoute()
  const { canAdmin } = useAuth()
  const inAdmin = path.startsWith('/admin')
  // 窄屏（<768）没有横向导航的位置：两个文字入口收进汉堡抽屉，
  // 顶栏只留一个「当前在第几层」的标题，避免出现竖排的「工/作/台」。
  const pageTitle = inAdmin ? '管理后台' : '工作台'

  return (
    <header className="topbar">
      {onMenu && (
        <button className="icon-btn menu-btn" onClick={onMenu} title="会话列表" aria-label="打开会话列表">
          <IconMenu />
        </button>
      )}
      <div className="brand">
        <span className="logo" />
        ASTRA
        <span className="brand-sub">Agent 工作台</span>
      </div>
      <span className="topbar-title" title={pageTitle}>
        {pageTitle}
      </span>

      <nav className="topnav">
        <a className={!inAdmin ? 'on' : ''} href="#/">
          工作台
        </a>
        {/* member 看不到管理入口。后端也会拒（403），但给一个必然失败的按钮
            只会让人以为系统坏了。 */}
        {canAdmin && (
          <a className={inAdmin ? 'on' : ''} href="#/admin/users">
            管理后台
          </a>
        )}
      </nav>

      <div className="chips">{chips}</div>

      <div className="topbar-right">
        {onSearch && (
          <button
            className="icon-btn search-btn"
            onClick={onSearch}
            title="搜索会话"
            aria-label="搜索会话"
          >
            <IconSearch />
          </button>
        )}
        {actions}
        <UserMenu />
      </div>
    </header>
  )
}

function UserMenu() {
  const { user, tenant, role, canAdmin, logout, changePassword, requireAuth } = useAuth()
  const [open, setOpen] = useState(false)
  const [pwdOpen, setPwdOpen] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  // 点空白处收起。不用 <details>：那玩意儿的展开状态没法在「点了菜单项之后」关掉。
  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (!boxRef.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  // 兼容模式没有真实用户，只有一个匿名的 dev 身份：这里如实说明，
  // 不编一个假邮箱让顶栏看起来「完整」。
  if (!user) {
    return (
      <span className="chip" title="服务端未启用认证，所有请求按本地全权身份处理">
        <b>身份</b>
        {requireAuth ? '未登录' : '本地模式'}
      </span>
    )
  }

  return (
    <div className="user" ref={boxRef}>
      <button
        className={`user-btn ${open ? 'on' : ''}`}
        onClick={() => setOpen((v) => !v)}
        title={tenant ? `${tenant.name}（${tenant.slug}）` : user.email}
      >
        <span className="user-avatar">{user.email.slice(0, 1).toUpperCase()}</span>
        <span className="user-mail">{user.email}</span>
        <span className="user-role">{ROLE_LABEL[role] || role}</span>
        <span className={`caret ${open ? 'open' : ''}`}>▶</span>
      </button>

      {open && (
        <div className="user-menu">
          <div className="user-menu-head">
            <div className="user-menu-mail">{user.email}</div>
            <div className="user-menu-meta">
              {ROLE_LABEL[role] || role}
              {tenant ? ` · ${tenant.name}` : ''}
            </div>
          </div>
          <button
            className="user-menu-item"
            onClick={() => {
              setOpen(false)
              setPwdOpen(true)
            }}
          >
            修改密码
          </button>
          {canAdmin && (
            <button
              className="user-menu-item"
              onClick={() => {
                setOpen(false)
                navigate('/admin/users')
              }}
            >
              管理后台
            </button>
          )}
          <button
            className="user-menu-item danger"
            onClick={() => {
              setOpen(false)
              void logout()
            }}
          >
            退出登录
          </button>
        </div>
      )}

      {pwdOpen && (
        <PasswordModal
          onClose={() => setPwdOpen(false)}
          onSubmit={changePassword}
        />
      )}
    </div>
  )
}

function PasswordModal({
  onClose,
  onSubmit,
}: {
  onClose: () => void
  onSubmit: (oldPassword: string, newPassword: string) => Promise<void>
}) {
  const [oldPwd, setOldPwd] = useState('')
  const [newPwd, setNewPwd] = useState('')
  const [again, setAgain] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [done, setDone] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    if (newPwd !== again) {
      setErr('两次输入的新密码不一致')
      return
    }
    setBusy(true)
    try {
      await onSubmit(oldPwd, newPwd)
      setDone(true)
    } catch (e2) {
      // 这个接口的 401 是「原密码不对」，不是会话失效（api 层已跳过跳转）
      if (e2 instanceof ApiError && e2.status === 401) setErr('原密码不正确')
      else setErr((e2 as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title="修改密码" onClose={onClose} width={400}>
      {done ? (
        <div className="stack">
          <Notice kind="info">
            密码已修改。当前登录仍然有效，下次登录请使用新密码。
          </Notice>
          <button className="btn-primary" onClick={onClose}>
            关闭
          </button>
        </div>
      ) : (
        <form onSubmit={submit} className="stack">
          <Field label="当前密码">
            <input
              className="input"
              type="password"
              autoComplete="current-password"
              value={oldPwd}
              onChange={(e) => setOldPwd(e.target.value)}
              required
            />
          </Field>
          <Field label="新密码" hint="至少 8 位">
            <input
              className="input"
              type="password"
              autoComplete="new-password"
              value={newPwd}
              onChange={(e) => setNewPwd(e.target.value)}
              minLength={8}
              required
            />
          </Field>
          <Field label="再输一次">
            <input
              className="input"
              type="password"
              autoComplete="new-password"
              value={again}
              onChange={(e) => setAgain(e.target.value)}
              required
            />
          </Field>
          {err && <Notice kind="error">{err}</Notice>}
          <div className="modal-actions">
            <button className="btn" type="button" onClick={onClose}>
              取消
            </button>
            <button className="btn-primary" type="submit" disabled={busy}>
              {busy ? '提交中…' : '确认修改'}
            </button>
          </div>
        </form>
      )}
    </Modal>
  )
}
