// 图标全部内联 SVG：不引图标库（一个库 300KB，而这里只用得到 9 个）。
const S = { width: 15, height: 15, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', strokeWidth: 1.7, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const }

export const IconPlus = () => (
  <svg {...S}>
    <path d="M12 5v14M5 12h14" />
  </svg>
)

export const IconTrash = () => (
  <svg {...S} width={13} height={13}>
    <path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v5M14 11v5" />
  </svg>
)

export const IconPencil = () => (
  <svg {...S} width={13} height={13}>
    <path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z" />
  </svg>
)

export const IconFolder = () => (
  <svg {...S}>
    <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
  </svg>
)

export const IconFile = () => (
  <svg {...S} width={14} height={14}>
    <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z" />
    <path d="M14 3v5h5" />
  </svg>
)

export const IconClose = () => (
  <svg {...S} width={14} height={14}>
    <path d="M6 6l12 12M18 6L6 18" />
  </svg>
)

export const IconSend = () => (
  <svg {...S} width={13} height={13}>
    <path d="M22 2 11 13M22 2l-7 20-4-9-9-4Z" />
  </svg>
)

export const IconStop = () => (
  <svg {...S} width={12} height={12}>
    <rect x="6" y="6" width="12" height="12" rx="1.5" fill="currentColor" stroke="none" />
  </svg>
)

export const IconRefresh = () => (
  <svg {...S} width={14} height={14}>
    <path d="M21 12a9 9 0 1 1-3-6.7M21 4v5h-5" />
  </svg>
)

/** 窄屏的会话抽屉开关。只在 <=760px 显示（.menu-btn 由 CSS 控制）。 */
export const IconMenu = () => (
  <svg {...S} width={18} height={18}>
    <path d="M3.5 6.5h17M3.5 12h17M3.5 17.5h17" />
  </svg>
)
