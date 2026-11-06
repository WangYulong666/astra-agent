// 语音控件用的图标。不复用 components/icons.tsx 里的那批：
// 那些是线性 16px 的通用图标，麦克风/扬声器需要更实的形状（录音中要变红、
// 要与 44px 的点击区匹配），混在一堆通用图标里反而不好维护。

const base = {
  width: 18,
  height: 18,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.8,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
}

export function IconMic({ off = false }: { off?: boolean }) {
  return (
    <svg {...base} aria-hidden="true" focusable="false">
      <rect x="9" y="2.5" width="6" height="11" rx="3" />
      <path d="M5.5 11a6.5 6.5 0 0 0 13 0" />
      <path d="M12 17.5V21" />
      {off && <path d="M3 3l18 18" />}
    </svg>
  )
}

export function IconSpeaker({ off = false }: { off?: boolean }) {
  return (
    <svg {...base} aria-hidden="true" focusable="false">
      <path d="M4 9.5h3.2L12 5.5v13L7.2 14.5H4z" />
      {!off && (
        <>
          <path d="M15.5 9a4.2 4.2 0 0 1 0 6" />
          <path d="M18 6.5a8 8 0 0 1 0 11" />
        </>
      )}
      {off && <path d="M16 9.5l5 5" />}
      {off && <path d="M21 9.5l-5 5" />}
    </svg>
  )
}

export function IconClose() {
  return (
    <svg {...base} width={14} height={14} aria-hidden="true" focusable="false">
      <path d="M6 6l12 12" />
      <path d="M18 6L6 18" />
    </svg>
  )
}
