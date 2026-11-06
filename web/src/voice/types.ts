// 语音相关的类型声明。
//
// 为什么自己写：`SpeechRecognition` 至今没有进 TypeScript 的 lib.dom.d.ts
// （只有 `SpeechSynthesis` 在里面），而各家的实现又都在 `webkit` 前缀下。
// 与其 `as any` 到处飞、把运行时错误留到浏览器里，不如把「我们真正用到的
// 那几个成员」声明出来——用错了 tsc 直接报，而不是线上静默。
//
// 声明严格贴着 MDN 的 Web Speech API 草案，只保留本目录实际使用的部分。

export type VoiceErrorCode =
  | 'not-allowed'
  | 'service-not-allowed'
  | 'no-speech'
  | 'network'
  | 'audio-capture'
  | 'language-not-supported'
  | 'aborted'
  | 'unknown'

export type VoiceError = {
  code: VoiceErrorCode
  /** 已经翻译成中文的说明，直接可以贴到界面上。 */
  message: string
}

export interface SpeechAlternativeLike {
  readonly transcript: string
  readonly confidence: number
}

export interface SpeechResultLike {
  readonly length: number
  readonly isFinal: boolean
  readonly [index: number]: SpeechAlternativeLike
}

export interface SpeechResultListLike {
  readonly length: number
  readonly [index: number]: SpeechResultLike
}

export interface SpeechRecognitionEventLike extends Event {
  readonly resultIndex: number
  readonly results: SpeechResultListLike
}

export interface SpeechRecognitionErrorEventLike extends Event {
  readonly error: string
  readonly message: string
}

/**
 * 识别器实例。
 *
 * 注意这里刻意声明成可变字段而不是构造参数：`lang` / `continuous` 可以在
 * 实例存活期间随时改，改完下一次 start() 生效——这正是我们要复用实例的
 * 前提（Safari 上反复 new 不会释放底层音频单元，几十次之后就开始出现
 * 无声、onend 不触发）。
 */
export interface SpeechRecognitionLike {
  lang: string
  continuous: boolean
  interimResults: boolean
  maxAlternatives: number
  start(): void
  stop(): void
  abort(): void
  onstart: ((ev: Event) => void) | null
  onaudiostart: ((ev: Event) => void) | null
  onend: ((ev: Event) => void) | null
  onerror: ((ev: SpeechRecognitionErrorEventLike) => void) | null
  onresult: ((ev: SpeechRecognitionEventLike) => void) | null
  onnomatch: ((ev: Event) => void) | null
}

export type SpeechRecognitionCtor = new () => SpeechRecognitionLike

/** 识别语言。只放两个：多一个选项就多一处要验证的音色/语法差异。 */
export type VoiceLang = 'zh-CN' | 'en-US'

export const VOICE_LANGS: { value: VoiceLang; label: string }[] = [
  { value: 'zh-CN', label: '中文' },
  { value: 'en-US', label: 'English' },
]

declare global {
  interface Window {
    SpeechRecognition?: SpeechRecognitionCtor
    webkitSpeechRecognition?: SpeechRecognitionCtor
  }
}
