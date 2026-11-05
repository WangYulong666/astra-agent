// 登录页与管理后台共用的小组件。
//
// 放在一个文件里而不是每页复制：这些是纯展示件，没有各自的业务语义，
// 复制六份之后「改一次弹层宽度」就得改六个地方。

import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import type { ReactNode } from 'react'

export function Modal({
  title,
  onClose,
  children,
  footer,
  width = 460,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  width?: number
}) {
  // Esc 关闭。没有这个，弹层里误点遮罩会丢草稿，但用户的第一反应还是按 Esc。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  // 必须挂到 body：顶栏带 backdrop-filter，它会让内部所有 position:fixed 的
  // 元素以顶栏为包含块——弹层会被挤成 52px 高的一条。这是最容易被忽略的
  // 一类样式副作用，用 portal 从根上避开。
  return createPortal(
    <div className="modal-mask" onClick={onClose}>
      <div
        className="modal"
        style={{ width }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-head">
          <span>{title}</span>
          <button className="mini-btn" onClick={onClose} title="关闭">
            ×
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>,
    document.body,
  )
}

export function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  )
}

/** 语义固定的提示条：错误用 err、警告用 warn、说明用 info。 */
export function Notice({
  kind = 'info',
  children,
}: {
  kind?: 'info' | 'warn' | 'error'
  children: ReactNode
}) {
  return <div className={`notice ${kind}`}>{children}</div>
}

/** 只出现一次的明文（初始密码、API Key）：给复制按钮，但提醒它不会再出现。 */
export function SecretOnce({
  value,
  label,
}: {
  value: string
  label: string
}) {
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
    } catch {
      // 非 https 或没授予剪贴板权限时会失败，用户仍可手动选中复制
    }
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div className="secret">
      <div className="secret-label">{label}</div>
      <div className="secret-row">
        <code>{value}</code>
        <button className="btn" onClick={() => void copy()}>
          {copied ? '已复制' : '复制'}
        </button>
      </div>
    </div>
  )
}

export function Empty({ text }: { text: string }) {
  return <div className="tbl-empty">{text}</div>
}

// ── 会话检索 / 消息操作 / 连接状态用到的图标 ──────────────────
//
// 归在 ui.tsx 而不是 icons.tsx：这一轮 icons.tsx 不在可改范围内（多人并行），
// 就近放几个只被本轮组件用到的图标，避免为了一个放大镜去动公共文件。
// 参数与 icons.tsx 保持一致（15px 描边图标），视觉上分不出来。

const S = {
  width: 15,
  height: 15,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.7,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
}

export const IconSearch = () => (
  <svg {...S} width={17} height={17}>
    <circle cx="10.5" cy="10.5" r="6.5" />
    <path d="m20 20-4.6-4.6" />
  </svg>
)

/** 图钉。filled 用于「已置顶」的常驻标记，未填充用于操作按钮。 */
export const IconPin = ({ filled }: { filled?: boolean }) => (
  <svg {...S} width={13} height={13} fill={filled ? 'currentColor' : 'none'}>
    <path d="M9.5 3h5l-.9 5.2 2.4 2.6V13H8v-2.2l2.4-2.6Z" />
    <path d="M12 13v8" />
  </svg>
)

export const IconCopy = () => (
  <svg {...S} width={13} height={13}>
    <rect x="9" y="9" width="11" height="11" rx="2.5" />
    <path d="M5 15V6a2 2 0 0 1 2-2h8" />
  </svg>
)

/** 重发：把消息重新放回输入框（不是立刻再发一次）。 */
export const IconReuse = () => (
  <svg {...S} width={13} height={13}>
    <path d="M4 4v6h6" />
    <path d="M4.7 14a8 8 0 1 0 .5-6.3L4 10" />
  </svg>
)

/**
 * 复制纯文本，返回是否成功。
 *
 * 不能只调 navigator.clipboard：手机上多半是通过局域网 IP（http://192.168.x.x）
 * 打开的，那不是安全上下文，clipboard API 直接不存在。给一个 textarea +
 * execCommand 的兜底，失败也如实返回 false，让调用方提示「请手动选中复制」。
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* 权限拒绝或非安全上下文：走下面的兜底 */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    // iOS 上 select() 之后还要显式设置选区，否则复制的是一整个空文本。
    ta.setSelectionRange(0, text.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}

export function Loading() {
  return (
    <div className="tbl-empty">
      <span className="spinner" /> 加载中
    </div>
  )
}
