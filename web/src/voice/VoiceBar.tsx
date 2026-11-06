import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { buildBlocks } from '../timeline'
import type { RunEvent } from '../types'
import { appendToComposer, submitComposer } from './composer'
import { IconClose, IconMic, IconSpeaker } from './icons'
import { plainForSpeech } from './speech'
import type { RecognitionResult } from './useSpeechRecognition'
import { useSpeechRecognition } from './useSpeechRecognition'
import { useSpeechSynthesis } from './useSpeechSynthesis'
import { VOICE_LANGS, type VoiceLang } from './types'

const LANG_KEY = 'astra_voice_lang'

function readLang(): VoiceLang {
  try {
    const v = window.localStorage.getItem(LANG_KEY)
    return v === 'en-US' ? 'en-US' : 'zh-CN'
  } catch {
    return 'zh-CN'
  }
}

function mmss(sec: number): string {
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

/**
 * 输入区上方的语音工具条：语音输入 + 朗读回复。
 *
 * 位置为什么在输入框「上方」而不是「里面」：Composer 是受控组件、不在本轮
 * 可改范围内（内部 state 才有输入框的值），要在 textarea 右侧塞一个按钮就得
 * 改它的 JSX。工具条落在输入框正上方、与输入框同宽右对齐，视觉上仍是
 * 「输入区的右侧」，而且窄屏下不会跟发送按钮抢宽度。
 */
export function VoiceBar({
  sessionId,
  runId,
  running,
  events,
}: {
  sessionId: string | null
  runId: string | null
  running: boolean
  events: RunEvent[]
}) {
  const [lang, setLang] = useState<VoiceLang>(readLang)
  const [notice, setNotice] = useState('')

  // 回复文本：只在 run 结束后整段交给 TTS，绝不逐 token 念。
  // 代码块、Markdown 符号先剥掉——念「星号星号 星号星号」没人受得了。
  const reply = useMemo(() => {
    const text = buildBlocks(events)
      .filter((b): b is { type: 'message'; text: string } => b.type === 'message')
      .map((b) => b.text)
      .join('\n')
    return plainForSpeech(text)
  }, [events])

  const tts = useSpeechSynthesis({ lang: 'zh-CN' })
  const { speak: speakReply, stop: stopSpeaking } = tts

  const commit = useCallback((r: RecognitionResult) => {
    const wrote = appendToComposer(r.text)
    if (!wrote) {
      setNotice('没找到输入框，这次识别的内容已丢弃')
      return
    }
    // 出错（断网/权限被收回）时只填进输入框、不自动发：
    // 让用户看一眼再说，总比把半句话发出去强。
    if (r.ok) submitComposer()
  }, [])

  const stt = useSpeechRecognition({ lang, onFinal: commit })

  // 提示条 6 秒后自己消失，不占着输入区上方的位置。
  useEffect(() => {
    if (!notice) return
    const t = window.setTimeout(() => setNotice(''), 6000)
    return () => window.clearTimeout(t)
  }, [notice])

  useEffect(() => {
    if (stt.error) setNotice(stt.error.message)
  }, [stt.error])

  // 切会话 / 起新 run 时立刻闭嘴。残留的朗读比没声音更让人困惑——
  // 你换了会话，耳朵里还是上一个 agent 在念。
  useEffect(() => {
    stopSpeaking()
  }, [sessionId, runId, stopSpeaking])

  // 只在「这一轮从进行中变成结束」的那一刻朗读完整回复。
  // spokenRun 防重：效果重跑（比如 events 更新导致 render）不会重念。
  const prevRunning = useRef(running)
  const spokenRun = useRef<string | null>(null)
  useEffect(() => {
    const was = prevRunning.current
    prevRunning.current = running
    if (!was || running || !runId) return
    if (spokenRun.current === runId) return
    spokenRun.current = runId
    if (reply) speakReply(reply)
  }, [running, runId, reply, speakReply])

  useEffect(() => {
    try {
      window.localStorage.setItem(LANG_KEY, lang)
    } catch {
      // ignore
    }
  }, [lang])

  const micBlocked = !stt.secure ? 'insecure' : !stt.supported ? 'unsupported' : null
  const micTitle =
    micBlocked === 'insecure'
      ? '当前页面不是安全上下文（需 HTTPS 或 localhost），浏览器不提供麦克风'
      : micBlocked === 'unsupported'
        ? '当前浏览器不支持语音输入'
        : stt.listening
          ? '结束录音并发送'
          : `点击开始语音输入（${lang === 'zh-CN' ? '中文' : 'English'}）`

  const ttsBlocked = !tts.supported
    ? '当前浏览器不支持语音合成'
    : tts.hasZhVoice === false
      ? '系统未安装中文语音包'
      : ''

  // 非安全上下文不是「用户点错了按钮」而是「这一整页用不了麦克风」，
  // 只在 tooltip 里说等于没说（手机上没有 hover）。这里常驻一行提示，
  // 顺手告诉他换个地址就能用——否则只会以为产品坏了。
  const insecureNotice =
    micBlocked === 'insecure'
      ? '当前页面不是安全上下文（http 非 localhost），浏览器不提供麦克风。用 https 或 127.0.0.1 打开即可。'
      : ''
  const shownNotice = notice || insecureNotice

  return (
    <div className="voice-bar">
      <div className="voice-bar-inner">
        {stt.listening && stt.interim && (
          <div className="voice-interim">
            <span className="voice-interim-text">{stt.interim}</span>
            <button className="voice-interim-cancel" onClick={stt.cancel} type="button">
              <IconClose />
              取消
            </button>
          </div>
        )}

        {shownNotice && (
          <div className="voice-notice" role="status">
            {shownNotice}
          </div>
        )}

        <div className="voice-controls">
          <select
            className="voice-lang"
            value={lang}
            onChange={(e) => setLang(e.target.value === 'en-US' ? 'en-US' : 'zh-CN')}
            disabled={Boolean(micBlocked)}
            aria-label="语音识别语言"
            title="识别语言（只在重新开始录音后生效）"
          >
            {VOICE_LANGS.map((l) => (
              <option key={l.value} value={l.value}>
                {l.label}
              </option>
            ))}
          </select>

          {stt.listening && (
            <span className="voice-timer" aria-live="off">
              <span className="voice-wave" aria-hidden="true">
                <i />
                <i />
                <i />
                <i />
              </span>
              {mmss(stt.seconds)}
            </span>
          )}

          <button
            type="button"
            className={`voice-mic${stt.listening ? ' listening' : ''}`}
            disabled={Boolean(micBlocked)}
            onClick={() => (stt.listening ? stt.stop() : stt.start())}
            title={micTitle}
            aria-label={micTitle}
            aria-pressed={stt.listening}
          >
            <IconMic off={Boolean(micBlocked)} />
          </button>

          <button
            type="button"
            className={`voice-tts${tts.enabled ? ' on' : ''}${tts.speaking ? ' speaking' : ''}`}
            disabled={Boolean(ttsBlocked)}
            onClick={() => tts.setEnabled(!tts.enabled)}
            title={
              ttsBlocked
                ? ttsBlocked
                : tts.enabled
                  ? tts.speaking
                    ? '正在朗读，点击停止'
                    : '朗读回复已开启，点击关闭'
                  : '朗读 agent 的回复（默认关闭）'
            }
            aria-label={tts.enabled ? '关闭朗读回复' : '开启朗读回复'}
            aria-pressed={tts.enabled}
          >
            <IconSpeaker off={!tts.enabled} />
            <span className="voice-tts-label">朗读</span>
          </button>
        </div>
      </div>
    </div>
  )
}
