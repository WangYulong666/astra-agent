import { describe, expect, it } from 'vitest'
import {
  BIG_FILE_BYTES,
  baseName,
  canShareFiles,
  classifyFile,
  describeDownloadError,
  downloadFilename,
  downloadUrl,
  extOf,
  formatSize,
  isImageName,
  isTextName,
  middleEllipsis,
  needsBigFileWarning,
  parseFilenameDisposition,
  progressPercent,
  sanitizeFilename,
} from './files'

describe('extOf / classifyFile：扩展名判断', () => {
  it('大小写不敏感，且不吃掉路径里的点', () => {
    expect(extOf('a.PNG')).toBe('png')
    expect(extOf('dir.v2/a.png')).toBe('png')
    expect(extOf('archive.tar.gz')).toBe('gz')
  })

  it('没有扩展名 / 点开头 / 点结尾都算没有扩展名', () => {
    expect(extOf('Makefile')).toBe('')
    expect(extOf('.env')).toBe('')
    expect(extOf('trailing.')).toBe('')
    expect(extOf('')).toBe('')
  })

  it('图片：只看扩展名，不看内容', () => {
    for (const n of ['x.png', 'x.jpg', 'x.JPEG', 'x.gif', 'x.webp', 'x.svg']) {
      expect(isImageName(n)).toBe(true)
      expect(classifyFile(n)).toBe('image')
    }
    expect(isImageName('x.png.txt')).toBe(false)
    expect(isImageName('png')).toBe(false)
  })

  it('文本白名单之外的一律二进制', () => {
    for (const n of ['main.go', 'report.md', 'data.jsonl', 'run.sh', 'Dockerfile', '.env']) {
      expect(isTextName(n)).toBe(true)
      expect(classifyFile(n)).toBe('text')
    }
    for (const n of ['app.zip', 'lib.dylib', 'model.bin', 'noext']) {
      expect(isTextName(n)).toBe(false)
      expect(classifyFile(n)).toBe('binary')
    }
  })

  it('目录优先于扩展名（名字叫 x.png 的目录不该当图片）', () => {
    expect(classifyFile('x.png', true)).toBe('dir')
  })
})

describe('formatSize：大小格式化', () => {
  it('分档与小数位', () => {
    expect(formatSize(0)).toBe('0 B')
    expect(formatSize(1023)).toBe('1023 B')
    expect(formatSize(1024)).toBe('1.0 KB')
    expect(formatSize(1536)).toBe('1.5 KB')
    expect(formatSize(1024 * 1024)).toBe('1.0 MB')
    expect(formatSize(5 * 1024 * 1024 + 512 * 1024)).toBe('5.5 MB')
    expect(formatSize(2 * 1024 * 1024 * 1024)).toBe('2.00 GB')
  })

  it('非法值不显示 NaN', () => {
    expect(formatSize(-1)).toBe('—')
    expect(formatSize(Number.NaN)).toBe('—')
    expect(formatSize(Number.POSITIVE_INFINITY)).toBe('—')
  })
})

describe('sanitizeFilename：路径穿越与非法字符', () => {
  it('路径穿越只留最后一段', () => {
    expect(sanitizeFilename('../etc/passwd')).toBe('passwd')
    expect(sanitizeFilename('../../.ssh/authorized_keys')).toBe('authorized_keys')
    expect(sanitizeFilename('..\\..\\boot.ini')).toBe('boot.ini')
    expect(sanitizeFilename('a/b/c.png')).toBe('c.png')
  })

  it('纯点点回落到默认名', () => {
    expect(sanitizeFilename('..')).toBe('download')
    expect(sanitizeFilename('.')).toBe('download')
    expect(sanitizeFilename('')).toBe('download')
    expect(sanitizeFilename('   ')).toBe('download')
    expect(sanitizeFilename('a/..')).toBe('download')
  })

  it('控制字符、引号、Windows 保留字符都不留', () => {
    expect(sanitizeFilename('a\r\nb.txt')).toBe('ab.txt')
    expect(sanitizeFilename('a\u0000b.txt')).toBe('ab.txt')
    expect(sanitizeFilename('a"b.txt')).toBe('a_b.txt')
    expect(sanitizeFilename('a<b>c?.txt')).toBe('a_b_c_.txt')
  })

  it('结尾的点与空格去掉（Windows 会静默截断）', () => {
    expect(sanitizeFilename('report.txt.')).toBe('report.txt')
    expect(sanitizeFilename('report ')).toBe('report')
  })

  it('保留中文与扩展名', () => {
    expect(sanitizeFilename('月度报告.png')).toBe('月度报告.png')
    expect(sanitizeFilename('截图 2026-11-04 15.20.01.png')).toBe('截图 2026-11-04 15.20.01.png')
  })

  it('超长名字截断但保住扩展名', () => {
    const long = `${'a'.repeat(400)}.md`
    const out = sanitizeFilename(long)
    expect(out.length).toBeLessThanOrEqual(180)
    expect(out.endsWith('.md')).toBe(true)
  })

  it('downloadFilename 从路径取名字', () => {
    expect(downloadFilename('src/out/result.csv')).toBe('result.csv')
    expect(downloadFilename('')).toBe('download')
  })
})

describe('baseName', () => {
  it('同时认 / 与 \\', () => {
    expect(baseName('a/b/c')).toBe('c')
    expect(baseName('a\\b\\c')).toBe('c')
    expect(baseName('c')).toBe('c')
  })
})

describe('middleEllipsis：长路径中间省略', () => {
  it('短于上限原样返回', () => {
    expect(middleEllipsis('a/b.png', 28)).toBe('a/b.png')
    expect(middleEllipsis('', 28)).toBe('')
  })

  it('长路径保留首尾并带省略号', () => {
    const p = 'very/long/nested/path/that/keeps/going/down/deep/report.png'
    const out = middleEllipsis(p, 20)
    expect(out.length).toBeLessThanOrEqual(20)
    expect(out).toContain('…')
    expect(out.startsWith('very')).toBe(true)
    // 尾部保住文件扩展名，否则省略后看不出是什么类型
    expect(out.endsWith('report.png')).toBe(true)
  })

  it('上限退化成 1 时只给省略号', () => {
    expect(middleEllipsis('abcdef', 1)).toBe('…')
  })
})

describe('progressPercent：进度百分比', () => {
  it('有总长度就算百分比并夹在 0..100', () => {
    expect(progressPercent(0, 200)).toBe(0)
    expect(progressPercent(50, 200)).toBe(25)
    expect(progressPercent(200, 200)).toBe(100)
    expect(progressPercent(300, 200)).toBe(100)
  })

  it('没有 Content-Length 返回 null（UI 走不确定态）', () => {
    expect(progressPercent(10, null)).toBeNull()
    expect(progressPercent(10, 0)).toBeNull()
    expect(progressPercent(10, undefined)).toBeNull()
  })
})

describe('下载策略与错误文案', () => {
  it('只有超大文件才需要先问一句', () => {
    expect(needsBigFileWarning(0)).toBe(false)
    expect(needsBigFileWarning(2 * 1024 * 1024)).toBe(false)
    expect(needsBigFileWarning(BIG_FILE_BYTES - 1)).toBe(false)
    expect(needsBigFileWarning(BIG_FILE_BYTES)).toBe(true)
  })

  it('401 / 404 / 网络错误各有各的说法', () => {
    expect(describeDownloadError(0)).toContain('网络不通')
    expect(describeDownloadError(401)).toContain('登录已过期')
    expect(describeDownloadError(404)).toContain('文件不存在')
    expect(describeDownloadError(403)).toContain('无权')
    expect(describeDownloadError(500)).toContain('服务端')
    expect(describeDownloadError(418)).toContain('418')
  })

  it('URL 编码路径与会话 id', () => {
    expect(downloadUrl('s 1', 'a b/中文.png')).toBe(
      '/v1/sessions/s%201/download?path=a%20b%2F%E4%B8%AD%E6%96%87.png',
    )
  })
})

describe('parseFilenameDisposition', () => {
  it('优先 filename*（RFC 5987）', () => {
    expect(parseFilenameDisposition(`attachment; filename="__.md"; filename*=UTF-8''%E6%8A%A5%E5%91%8A.md`)).toBe(
      '报告.md',
    )
  })

  it('退回 filename=', () => {
    expect(parseFilenameDisposition('attachment; filename="a.txt"')).toBe('a.txt')
    expect(parseFilenameDisposition('attachment; filename=a.txt')).toBe('a.txt')
  })

  it('没有头或编码坏了都不抛', () => {
    expect(parseFilenameDisposition(null)).toBe('')
    expect(parseFilenameDisposition('inline')).toBe('')
    expect(parseFilenameDisposition("attachment; filename*=UTF-8''%E6%8A")).toBe('')
  })
})

describe('canShareFiles：分享能力判断', () => {
  const file = {} as File

  it('navigator 缺 share / canShare 都算不支持', () => {
    expect(canShareFiles(undefined, [file])).toBe(false)
    expect(canShareFiles({}, [file])).toBe(false)
    expect(canShareFiles({ share: async () => {} }, [file])).toBe(false)
  })

  it('以 canShare 的返回值为准', () => {
    expect(canShareFiles({ share: async () => {}, canShare: () => true }, [file])).toBe(true)
    expect(canShareFiles({ share: async () => {}, canShare: () => false }, [file])).toBe(false)
  })

  it('canShare 抛错时按不支持处理', () => {
    const nav = {
      share: async () => {},
      canShare: () => {
        throw new Error('boom')
      },
    }
    expect(canShareFiles(nav, [file])).toBe(false)
  })
})
