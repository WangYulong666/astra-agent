import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { pickVoice, splitSentences } from './speech'
import type { VoiceLang } from './types'

const TTS_KEY = 'astra_voice_tts'

function readEnabled(): boolean {
  try {
    return window.localStorage.getItem(TTS_KEY) === '1'
  } catch {
    // 隐私模式 / 存储被禁：默认关。突然出声比「开关记不住」吓人得多。
    return false
  }
}

export type UseSpeechSynthesis = {
  supported: boolean
  enabled: boolean
  setEnabled: (v: boolean) => void
  /** 正在朗读（含句间空隙）。 */
  speaking: boolean
  /** 系统里有没有中文音色。null = 还没加载完，别急着置灰。 */
  hasZhVoice: boolean | null
  /** 实际选中的音色名，用来在界面上告诉用户「现在是谁在念」。 */
  voiceName: string
  speak: (text: string) => void
  stop: () => void
}

/**
 * 语音合成（TTS）。
 *
 * 关键点：
 *
 * 1. **默认关闭**。浏览器一进页面就出声是非常冒犯的行为（开会、深夜、
 *    共享屏幕时）。开关状态存 localStorage，用户主动打开过就记住。
 *
 * 2. **按句串行**。Chrome 对单条 utterance 有 ~15 秒的隐形上限，超时会
 *    静默暂停；而且一次性把整篇长文丢进去，首字延迟会非常明显。切成句子
 *    一条一条 speak，用上一条的 onend 驱动下一条，延迟和截断都能规避。
 *
 * 3. **generation 计数**。speechSynthesis 是全局单例，cancel() 是异步的：
 *    取消时当前 utterance 会回一个 error('interrupted')，如果那个回调还在
 *    推进队列，就会出现「点了停止还在念下一句」。每次 speak/stop 都递增
 *    generation，回调里对不上就直接返回。
 *
 * 4. **只在 run 结束后朗读完整回复**。逐 token 念会把句子切碎，听感极差，
 *    也会让 cancel 变得无法收敛。调用方负责这个时序。
 */
export function useSpeechSynthesis({ lang }: { lang: VoiceLang }): UseSpeechSynthesis {
  const supported = useMemo(
    () => typeof window !== 'undefined' && 'speechSynthesis' in window,
    [],
  )

  const [enabled, setEnabledState] = useState<boolean>(readEnabled)
  const [speaking, setSpeaking] = useState(false)
  const [voices, setVoices] = useState<SpeechSynthesisVoice[]>([])

  const genRef = useRef(0)
  const queueRef = useRef<string[]>([])
  const cursorRef = useRef(0)

  // 音色列表在部分浏览器是异步填充的（Chrome 第一次返回空数组）。
  // 不用轮询：voiceschanged 是标准事件，Chrome/Safari/Firefox 都会发。
  useEffect(() => {
    if (!supported) return
    const synth = window.speechSynthesis
    const load = () => setVoices(synth.getVoices())
    load()
    synth.addEventListener('voiceschanged', load)
    return () => synth.removeEventListener('voiceschanged', load)
  }, [supported])

  const zhVoice = useMemo(() => (voices.length ? pickVoice(voices, 'zh-CN') : null), [voices])
  const hasZhVoice = voices.length === 0 ? null : Boolean(pickVoice(voices, 'zh-CN'))

  const stop = useCallback(() => {
    if (!supported) return
    genRef.current += 1
    queueRef.current = []
    cursorRef.current = 0
    setSpeaking(false)
    try {
      window.speechSynthesis.cancel()
    } catch {
      // ignore
    }
  }, [supported])

  const playFrom = useCallback(
    (gen: number) => {
      if (gen !== genRef.current) return
      const synth = window.speechSynthesis
      const q = queueRef.current
      if (cursorRef.current >= q.length) {
        setSpeaking(false)
        return
      }
      const text = q[cursorRef.current]

      const u = new SpeechSynthesisUtterance(text)
      u.lang = lang
      // 现取一次：state 里那份可能是上一帧的快照，用户刚装完语音包时
      // 现取的列表才是对的。
      const v = pickVoice(synth.getVoices(), lang)
      if (v) u.voice = v
      // 语速略快于 1：中文 TTS 默认偏拖，1.05 更接近正常语速。
      u.rate = 1.05
      u.pitch = 1

      u.onend = () => {
        if (gen !== genRef.current) return
        cursorRef.current += 1
        playFrom(gen)
      }
      u.onerror = () => {
        // 被 cancel 打断是正常路径，不要再往后念。
        if (gen !== genRef.current) return
        cursorRef.current += 1
        playFrom(gen)
      }

      // Chrome 偶发把合成器停在 paused 状态（切后台、系统休眠后）。
      if (synth.paused) synth.resume()
      synth.speak(u)
    },
    [lang],
  )

  const speak = useCallback(
    (text: string) => {
      if (!supported) return
      const enabledNow = enabled
      if (!enabledNow) return
      const sentences = splitSentences(text).slice(0, 80)
      if (!sentences.length) return

      genRef.current += 1
      const gen = genRef.current
      queueRef.current = sentences
      cursorRef.current = 0
      setSpeaking(true)
      // cancel() 是异步的：紧跟着 speak() 有概率被丢掉（Chrome 上表现为
      // 「第一次点没声音，第二次才响」）。让出一帧再开始。
      try {
        window.speechSynthesis.cancel()
      } catch {
        // ignore
      }
      window.setTimeout(() => playFrom(gen), 0)
    },
    [supported, enabled, playFrom],
  )

  const setEnabled = useCallback(
    (v: boolean) => {
      setEnabledState(v)
      try {
        window.localStorage.setItem(TTS_KEY, v ? '1' : '0')
      } catch {
        // ignore
      }
      if (!v) {
        stop()
        return
      }
      // Safari 要求 speak() 由用户手势触发。这个 toggle 的点击就是手势，
      // 顺手放一条静音 utterance 把合成器「解锁」，后面 run 结束时的
      // 自动朗读才不会被静默拒绝。
      if (!supported) return
      try {
        const u = new SpeechSynthesisUtterance(' ')
        u.volume = 0
        window.speechSynthesis.speak(u)
      } catch {
        // ignore
      }
    },
    [supported, stop],
  )

  // 卸载时一定要 cancel：单例合成器不会因为组件没了就闭嘴，
  // 路由切换后还在念上一页的内容是很典型的 bug。
  useEffect(() => {
    return () => {
      if (!supported) return
      try {
        window.speechSynthesis.cancel()
      } catch {
        // ignore
      }
    }
  }, [supported])

  return {
    supported,
    enabled,
    setEnabled,
    speaking,
    hasZhVoice,
    voiceName: zhVoice?.name ?? '',
    speak,
    stop,
  }
}
