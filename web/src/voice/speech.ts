import type { SpeechRecognitionCtor, VoiceError, VoiceErrorCode } from './types'

// 纯函数集中放这里：不碰 DOM、不碰 React，方便直接写单测。
// 浏览器相关的探测（拿到构造函数、判断安全上下文）也放这儿，
// 因为它们是「能力查询」，跟状态机无关。

/** 取识别器构造函数。Chrome/Edge 走 webkit 前缀，Safari 也是前缀。 */
export function getRecognitionCtor(): SpeechRecognitionCtor | null {
  if (typeof window === 'undefined') return null
  return window.SpeechRecognition ?? window.webkitSpeechRecognition ?? null
}

/**
 * 是否安全上下文。
 *
 * 麦克风属于「强能力」，Chrome/Safari/Firefox 都只在 secure context 下暴露
 * （https 或 localhost/127.0.0.1）。用 http://192.168.x.x 打开时构造函数
 * 干脆不存在——如果只判断构造函数，用户会看到「不支持语音输入」，其实
 * 换个地址就能用。所以两种情况分开提示。
 */
export function isSecure(): boolean {
  if (typeof window === 'undefined') return false
  if (window.isSecureContext) return true
  // 老浏览器没有 isSecureContext，退化成看协议。
  return location.protocol === 'https:' || location.hostname === 'localhost'
}

const ERROR_TEXT: Record<string, string> = {
  'not-allowed': '没有麦克风权限。点地址栏左侧的图标，允许麦克风后重试。',
  'service-not-allowed': '浏览器禁用了语音识别服务（可能被系统隐私设置或企业策略关掉了）。',
  'no-speech': '没听到声音。靠近麦克风，等按钮变红之后再说话。',
  network: '语音识别服务连不上。Chrome 的识别在云端完成，断网就用不了。',
  'audio-capture': '找不到麦克风。检查设备是否插好、是否被别的程序占用。',
  'language-not-supported': '浏览器不支持当前选择的识别语言。',
  'bad-grammar': '识别语法加载失败，换个语言或刷新页面重试。',
}

/**
 * 错误码 → 人话。
 *
 * 浏览器给的 `error` 是一串 kebab-case 的枚举值，直接展示等于没说。
 * 这里逐条翻译，并保留原始码放在 VoiceError.code 里，方便排查时对日志。
 * 未知码兜底成「语音输入出错（xxx）」，至少还留着原材料。
 */
export function humanizeRecognitionError(code: string): VoiceError {
  const text = ERROR_TEXT[code]
  if (text) return { code: code as VoiceErrorCode, message: text }
  return { code: 'unknown', message: `语音输入出错（${code || '未知'}）。` }
}

/**
 * 按句切分，供 TTS 逐句朗读。
 *
 * 为什么必须切：Chrome 对单条 utterance 有大约 15 秒的隐形上限，超时后
 * 会自己暂停（而且 paused 状态不一定会派发事件），用户听到的就是「念到
 * 一半没了」。按句喂进去，每句都在几秒内结束，问题就不会累积。
 *
 * 切分字符：中文句号/感叹/问号、英文 . ! ?、以及换行。
 * 两个细节：
 *   1. 小数点不当句末（3.14 不能切成两句）；
 *   2. 连续标点（「？！」）不产生空句；
 *   3. 单句超过 maxLen 时再按逗号/顿号兜底切一刀——模型经常一口气写 200 字
 *      不带句号，光靠句末标点切不开。
 */
export function splitSentences(text: string, maxLen = 90): string[] {
  const src = text.replace(/\r\n?/g, '\n')
  const out: string[] = []
  let buf = ''

  const flush = () => {
    const t = buf.trim()
    buf = ''
    if (!t) return
    // 只剩标点的片段（「？！」的后半截）并回上一句：单独一条「！」喂给
    // TTS 要么没声音，要么读成「感叹号」。
    if (out.length && !/[\p{L}\p{N}]/u.test(t)) {
      out[out.length - 1] += t
      return
    }
    out.push(t)
  }

  for (let i = 0; i < src.length; i++) {
    const ch = src[i]
    buf += ch
    let boundary = false
    if (ch === '。' || ch === '！' || ch === '？' || ch === '!' || ch === '?' || ch === '\n') {
      boundary = true
    } else if (ch === '.') {
      // 后面紧跟数字 = 小数点；紧跟字母 = 缩写/域名，都不当句末。
      const next = src[i + 1] ?? ''
      boundary = !/[\dA-Za-z]/.test(next)
    }
    if (boundary) flush()
  }
  flush()

  // 超长句兜底：按次级标点再切，仍然超长就硬切。
  const result: string[] = []
  for (const s of out) {
    if (s.length <= maxLen) {
      result.push(s)
      continue
    }
    let piece = ''
    for (const ch of s) {
      piece += ch
      if (piece.length >= maxLen || '，、；,;：:'.includes(ch)) {
        const t = piece.trim()
        if (t) result.push(t)
        piece = ''
      }
    }
    const tail = piece.trim()
    if (tail) result.push(tail)
  }
  return result
}

/**
 * 挑一个中文音色。
 *
 * 优先级：local（不用联网、延迟低、断网也能念）→ 任意 zh。
 * Chrome 桌面端通常同时有「Google 普通话（云端）」和系统音色，云端那个
 * 在弱网下首字延迟能到 1 秒以上，所以本地优先。
 */
export function pickVoice<T extends { lang: string; localService?: boolean; name: string }>(
  voices: T[],
  lang: string,
): T | null {
  const want = lang.toLowerCase()
  const prefix = want.split('-')[0]
  const match = voices.filter((v) => {
    const l = (v.lang || '').toLowerCase().replace('_', '-')
    return l === want || l.startsWith(prefix + '-') || l === prefix
  })
  if (!match.length) return null
  const local = match.find((v) => v.localService)
  return local ?? match[0]
}

/**
 * 把 Markdown 正文压成「能念」的纯文本。
 *
 * agent 的回复里经常整段是代码、表格、diff。让 TTS 念这些不只是难听——
 * 代码里的 `_` `*` 会被读成「下划线」「星号」连成一片，几百字念下来
 * 用户完全抓不到重点。宁可整块丢掉，只念自然语言部分。
 */
export function plainForSpeech(text: string): string {
  return text
    // 围栏代码块整块丢
    .replace(/```[\s\S]*?```/g, ' ')
    // 行内代码保留内容（多是命令名、变量名，念出来还有意义）
    .replace(/`([^`\n]+)`/g, '$1')
    // 图片丢、链接留文字
    .replace(/!\[[^\]]*\]\([^)]*\)/g, ' ')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    // HTML 标签
    .replace(/<[^>]+>/g, ' ')
    // 表格分隔行
    .replace(/^\s*\|?[\s:-]*\|[\s:|-]*$/gm, ' ')
    // 行首标记（标题、引用、列表、任务框）
    .replace(/^\s{0,3}(#{1,6}\s+|>\s?|[-*+]\s+|\d+[.)]\s+|\[[ xX]\]\s+)/gm, '')
    // 强调/删除线符号
    .replace(/(\*\*|__|~~|\*|_)/g, '')
    // 水平线
    .replace(/^\s*([-*_]\s*){3,}$/gm, ' ')
    // 多余空白
    .replace(/[ \t]+/g, ' ')
    .replace(/\n{3,}/g, '\n\n')
    .trim()
}

/** 把识别/提交的文本接到已有输入后面。中文之间不加空格，英文之间加。 */
export function joinText(prev: string, add: string): string {
  const a = prev.replace(/\s+$/, '')
  const b = add.trim()
  if (!a) return b
  if (!b) return a
  const needSpace = /[A-Za-z0-9]$/.test(a) && /^[A-Za-z0-9]/.test(b)
  return needSpace ? `${a} ${b}` : `${a}${b}`
}
