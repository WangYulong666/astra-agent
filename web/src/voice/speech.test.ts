import { describe, expect, it } from 'vitest'
import {
  humanizeRecognitionError,
  joinText,
  pickVoice,
  plainForSpeech,
  splitSentences,
} from './speech'

describe('splitSentences', () => {
  it('按中英文句末标点切开', () => {
    expect(splitSentences('你好。今天天气不错！要出门吗？')).toEqual([
      '你好。',
      '今天天气不错！',
      '要出门吗？',
    ])
    expect(splitSentences('Hello. How are you? Fine!')).toEqual([
      'Hello.',
      'How are you?',
      'Fine!',
    ])
  })

  it('小数点不切开', () => {
    expect(splitSentences('圆周率是 3.14 左右。')).toEqual(['圆周率是 3.14 左右。'])
    expect(splitSentences('版本 v1.2.3 已发布。')).toEqual(['版本 v1.2.3 已发布。'])
  })

  it('换行也算句末，空行不产生空句', () => {
    expect(splitSentences('第一行\n\n第二行')).toEqual(['第一行', '第二行'])
  })

  it('连续标点不产生空串', () => {
    expect(splitSentences('真的吗？！太好了一一')).toEqual(['真的吗？！', '太好了一一'])
  })

  it('超长无标点的句子会按逗号兜底切开', () => {
    const long = `${'啊'.repeat(50)}，${'啊'.repeat(50)}，${'啊'.repeat(50)}`
    const out = splitSentences(long, 90)
    expect(out.length).toBeGreaterThan(1)
    for (const s of out) expect(s.length).toBeLessThanOrEqual(90)
  })

  it('空输入返回空数组', () => {
    expect(splitSentences('')).toEqual([])
    expect(splitSentences('   \n  ')).toEqual([])
  })
})

describe('plainForSpeech', () => {
  it('丢掉围栏代码块', () => {
    const src = '先看这段：\n```go\nfunc main() {}\n```\n然后就好了。'
    const out = plainForSpeech(src)
    expect(out).not.toContain('func main')
    expect(out).toContain('先看这段')
    expect(out).toContain('然后就好了')
  })

  it('行内代码保留内容、去掉反引号', () => {
    expect(plainForSpeech('运行 `npm test` 即可')).toBe('运行 npm test 即可')
  })

  it('链接留文字，图片整条丢掉', () => {
    expect(plainForSpeech('见[文档](https://x.dev)')).toBe('见文档')
    expect(plainForSpeech('看这个 ![截图](/a.png) 就好')).toBe('看这个 就好')
  })

  it('去掉标题/列表/强调标记', () => {
    expect(plainForSpeech('## 结论\n- **重要**：别动\n* 其次')).toBe('结论\n重要：别动\n其次')
  })
})

describe('joinText', () => {
  it('中文之间不加空格', () => {
    expect(joinText('帮我看看', '这个目录')).toBe('帮我看看这个目录')
  })

  it('英文之间补一个空格', () => {
    expect(joinText('hello', 'world')).toBe('hello world')
  })

  it('任一侧为空时直接取另一侧', () => {
    expect(joinText('', '你好')).toBe('你好')
    expect(joinText('你好', '   ')).toBe('你好')
  })

  it('已有内容和语音内容边界上的空白会被收掉', () => {
    expect(joinText('看这里   ', '  然后呢')).toBe('看这里然后呢')
  })
})

describe('humanizeRecognitionError', () => {
  it('已知错误码翻译成具体建议', () => {
    expect(humanizeRecognitionError('not-allowed').message).toContain('麦克风权限')
    expect(humanizeRecognitionError('no-speech').message).toContain('没听到')
    expect(humanizeRecognitionError('network').message).toContain('连不上')
    expect(humanizeRecognitionError('audio-capture').message).toContain('麦克风')
  })

  it('未知错误码保留原始值并兜底', () => {
    const e = humanizeRecognitionError('weird-thing')
    expect(e.code).toBe('unknown')
    expect(e.message).toContain('weird-thing')
  })
})

describe('pickVoice', () => {
  const voices = [
    { name: 'English US', lang: 'en-US', localService: true },
    { name: 'Tingting Cloud', lang: 'zh-CN', localService: false },
    { name: 'Tingting Local', lang: 'zh-CN', localService: true },
  ]

  it('优先本地中文音色', () => {
    expect(pickVoice(voices, 'zh-CN')?.name).toBe('Tingting Local')
  })

  it('没有本地音色时退回任意匹配', () => {
    expect(pickVoice([voices[1]], 'zh-CN')?.name).toBe('Tingting Cloud')
  })

  it('没有对应语言时返回 null', () => {
    expect(pickVoice([voices[0]], 'zh-CN')).toBeNull()
  })

  it('lang 用下划线的实现也能识别（部分安卓版返回 zh_CN）', () => {
    expect(pickVoice([{ name: 'X', lang: 'zh_CN', localService: true }], 'zh-CN')?.name).toBe('X')
  })
})
