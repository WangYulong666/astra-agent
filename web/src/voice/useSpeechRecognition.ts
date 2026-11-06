import { useCallback, useEffect, useRef, useState } from 'react'
import { getRecognitionCtor, humanizeRecognitionError, isSecure } from './speech'
import type { SpeechRecognitionLike, VoiceError, VoiceLang } from './types'

export type RecognitionResult = {
  text: string
  /** true = 正常说完；false = 被打断/出错，调用方不应该自动发送。 */
  ok: boolean
}

export type UseSpeechRecognition = {
  /** 浏览器有没有这个能力（不含权限）。 */
  supported: boolean
  /** 页面是不是安全上下文；false 时 supported 也一定是 false。 */
  secure: boolean
  listening: boolean
  /** 实时中间结果，松手/停顿后会变；用于输入框上方那行灰字。 */
  interim: string
  /** 已识别但还没定稿的最终文本（停止后由 onFinal 一次性交出去）。 */
  error: VoiceError | null
  /** 录音计时（秒），给「录音中」的计时显示用。 */
  seconds: number
  start: () => void
  /** 正常结束：会把已识别的最终文本交给 onFinal 并允许自动发送。 */
  stop: () => void
  /** 丢弃：abort 掉，已识别的文本也不交出去。 */
  cancel: () => void
}

/**
 * 语音识别（STT）。
 *
 * 几个不显然的决定：
 *
 * 1. **实例复用**。识别器只 new 一次，之后 start/stop 都作用在同一个对象上。
 *    Safari（尤其是 iOS）每次 new 都会占一份底层音频资源，反复创建之后
 *    onend 不再触发、或者干脆静音——这类泄漏在开发机上很难复现，到手机上
 *    才炸。所以宁可处理「同一个实例能不能再次 start」的边界（try/catch +
 *    状态守卫），也不每次 new。
 *
 * 2. **continuous = false**。连续识别在 Chrome 上会一直占着麦克风不放，
 *    并且在静音超过几秒后自己结束；对「说一句话提问」这个场景没有意义。
 *    代价是每次说话都要重新点一下按钮（Safari 还要求每次都是用户手势）。
 *
 * 3. **interimResults = true**。识别延迟 0.5~2s，没有中间结果的话用户不知道
 *    到底开没开始录。中间结果只用于展示，不写进输入框——写进去再回滚会
 *    把用户手打的字搅乱。
 *
 * 4. **onend 是唯一的收尾入口**。Chrome 在 stop() 和自然结束时都走 onend，
 *    但 onerror 之后也会走 onend；所有状态清理（listening / interim / 计时）
 *    都放在这里，避免某一分支漏清导致按钮永远红着。
 */
export function useSpeechRecognition({
  lang,
  onFinal,
}: {
  lang: VoiceLang
  onFinal: (r: RecognitionResult) => void
}): UseSpeechRecognition {
  const ctorRef = useRef(getRecognitionCtor())
  const recRef = useRef<SpeechRecognitionLike | null>(null)
  // 最终文本按事件累加，onend 时一次性交出去。
  const finalRef = useRef('')
  // 这次是不是「用户主动取消」——取消就不提交。
  const cancelRef = useRef(false)
  // 上一次的 error code。onerror 先于 onend 触发，但 setState 是异步的，
  // 用 ref 才能在 onend 里同步读到。
  const errorRef = useRef<VoiceError | null>(null)

  const [listening, setListening] = useState(false)
  const [interim, setInterim] = useState('')
  const [error, setError] = useState<VoiceError | null>(null)
  const [seconds, setSeconds] = useState(0)

  // 回调走 ref：onFinal 每次 render 都是新函数，如果进依赖数组，
  // 识别器就得跟着重建——正好是我们要避免的事。
  const onFinalRef = useRef(onFinal)
  onFinalRef.current = onFinal

  const secure = isSecure()
  const supported = Boolean(ctorRef.current) && secure

  useEffect(() => {
    const Ctor = ctorRef.current
    if (!Ctor) return

    const rec = new Ctor()
    rec.continuous = false
    rec.interimResults = true
    rec.maxAlternatives = 1
    recRef.current = rec

    rec.onstart = () => {
      setListening(true)
      setSeconds(0)
    }

    rec.onresult = (ev) => {
      let live = ''
      for (let i = ev.resultIndex; i < ev.results.length; i++) {
        const r = ev.results[i]
        const t = r[0]?.transcript ?? ''
        if (!t) continue
        if (r.isFinal) finalRef.current += t
        else live += t
      }
      setInterim(live)
    }

    rec.onerror = (ev) => {
      // aborted 是我们自己 cancel() 造成的，不该弹错。
      if (ev.error === 'aborted') return
      const e = humanizeRecognitionError(ev.error)
      errorRef.current = e
      setError(e)
    }

    rec.onend = () => {
      setListening(false)
      setInterim('')

      const text = finalRef.current.trim()
      finalRef.current = ''
      const cancelled = cancelRef.current
      cancelRef.current = false

      if (!text) return
      // 出错时把已有文本当「可以填进输入框但不能自动发送」处理：
      // 用户说了半天，结果因为网络抖一下全丢了，比多发一条半句话更糟。
      onFinalRef.current({ text, ok: !cancelled && !errorRef.current })
    }

    return () => {
      rec.onstart = null
      rec.onresult = null
      rec.onerror = null
      rec.onend = null
      try {
        rec.abort()
      } catch {
        // 某些实现里没 start 过就 abort 会抛，忽略。
      }
      recRef.current = null
    }
  }, [])

  // 语言切换不需要重建实例：lang 是实例上的可写字段，下次 start 生效。
  useEffect(() => {
    const rec = recRef.current
    if (rec) rec.lang = lang
  }, [lang])

  // 录音计时。只在 listening 时开定时器，停就清——不用 requestAnimationFrame，
  // 1s 一次足够，也不在后台上白烧 CPU。
  useEffect(() => {
    if (!listening) return
    const t = window.setInterval(() => setSeconds((s) => s + 1), 1000)
    return () => window.clearInterval(t)
  }, [listening])

  const start = useCallback(() => {
    const rec = recRef.current
    if (!rec) return
    setError(null)
    errorRef.current = null
    finalRef.current = ''
    cancelRef.current = false
    setInterim('')
    rec.lang = lang
    try {
      rec.start()
    } catch {
      // 已经处于 started 状态时会抛 InvalidStateError。先 stop 一次再起，
      // 比直接吞掉错误更接近用户的意图（他就是要开始录）。
      try {
        rec.stop()
      } catch {
        // ignore
      }
    }
  }, [lang])

  const stop = useCallback(() => {
    const rec = recRef.current
    if (!rec) return
    cancelRef.current = false
    try {
      // stop 会先把已有的 final 结果派发完再触发 onend，比 abort 温和。
      rec.stop()
    } catch {
      // ignore
    }
  }, [])

  const cancel = useCallback(() => {
    const rec = recRef.current
    if (!rec) return
    cancelRef.current = true
    finalRef.current = ''
    setInterim('')
    setListening(false)
    try {
      rec.abort()
    } catch {
      // ignore
    }
  }, [])

  return { supported, secure, listening, interim, error, seconds, start, stop, cancel }
}
