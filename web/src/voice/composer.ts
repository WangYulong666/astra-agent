import { joinText } from './speech'

/**
 * 把语音结果写进 Composer 的输入框，并复用 Composer 自己的提交逻辑。
 *
 * 为什么要碰 DOM：Composer 是受控组件，输入框的 value 存在它自己的 state 里，
 * 外部没有受支持的写入通道。直接 `el.value = text` 会被 React 的 value
 * tracker 判成「值没变」而吞掉 onChange，所以必须走原型上的原生 setter
 * 再补一个 input 事件——这是 React 官方 issue 里给出的做法，也是这个页面
 * 里「示例指令填入输入框」已经在用的同一套写法（见 App.tsx 的 fillComposer）。
 *
 * 提交不自己拼一个 send()：Composer 的 submit 会顺带处理 trim、输入法状态、
 * 清空输入框、running 时切到「停止」等分支。复制一份逻辑迟早会跟它漂移。
 */

const COMPOSER_SELECTOR = '.composer textarea'

function textarea(): HTMLTextAreaElement | null {
  return document.querySelector<HTMLTextAreaElement>(COMPOSER_SELECTOR)
}

/** 追加（不是覆盖）到输入框末尾。返回是否真的写进去了。 */
export function appendToComposer(text: string): boolean {
  const el = textarea()
  if (!el) return false
  const next = joinText(el.value, text)
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set
  setter?.call(el, next)
  el.dispatchEvent(new Event('input', { bubbles: true }))
  el.focus()
  // 光标落到末尾：用户接着手动补一句时不用再点一下。
  try {
    el.setSelectionRange(next.length, next.length)
  } catch {
    // ignore
  }
  return true
}

/**
 * 触发 Composer 的发送。
 *
 * 写入之后 React 要渲染一帧发送按钮才会从 disabled 变可点，所以轮询几帧
 * 而不是立刻 click。run 正在跑时这一格是「停止」按钮，选择器匹配不到，
 * 自然就不会误触。
 */
export function submitComposer(timeoutMs = 800): void {
  const deadline = performance.now() + timeoutMs
  const tick = () => {
    const btn = document.querySelector<HTMLButtonElement>('.composer .send')
    if (btn && !btn.disabled) {
      btn.click()
      return
    }
    if (performance.now() > deadline) return
    requestAnimationFrame(tick)
  }
  requestAnimationFrame(tick)
}
