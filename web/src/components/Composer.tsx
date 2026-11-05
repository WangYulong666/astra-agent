import { useEffect, useRef, useState } from 'react'
import { IconSend, IconStop } from './icons'

export function Composer({
  running,
  disabled,
  onSend,
  onStop,
}: {
  running: boolean
  disabled: boolean
  onSend: (text: string) => void
  onStop: () => void
}) {
  const [text, setText] = useState('')
  const ref = useRef<HTMLTextAreaElement>(null)

  // 输入框跟着内容长高，但封顶在 CSS 的 max-height。
  useEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 220)}px`
  }, [text])

  const submit = () => {
    const t = text.trim()
    if (!t || disabled) return
    onSend(t)
    setText('')
  }

  return (
    <div className="composer-wrap">
      <div className="composer">
        <textarea
          ref={ref}
          value={text}
          placeholder="下一条指令…（命令会在沙箱里执行）"
          disabled={disabled}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            // 中文输入法选词时的回车不能当发送：不判断 isComposing 的话，
            // 每次选完词都会把半句话发出去。
            if (e.key === 'Enter' && !e.shiftKey) {
              if (e.nativeEvent.isComposing) return
              e.preventDefault()
              submit()
            }
          }}
        />
        <div className="composer-bar">
          <span className="composer-hint">Enter 发送 · Shift+Enter 换行</span>
          {running ? (
            <button className="stop" onClick={onStop} title="停止这一轮">
              <IconStop />
              停止
            </button>
          ) : (
            <button className="send" onClick={submit} disabled={!text.trim() || disabled}>
              <IconSend />
              发送
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
