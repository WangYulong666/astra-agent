// 文件产出层的纯逻辑 + 少量浏览器副作用。
//
// 为什么单独一个模块而不是塞进 FilePanel：这里每一个判断都会被「手机上文件
// 下不下来」这种结果检验，函数混在组件里就只能靠点界面来验证。抽出来之后
// 扩展名判断、大小格式化、文件名清洗都能直接单测。
//
// 依赖方向：files.ts 不 import 任何组件，只被 FilePanel 引用。

export type FileKind = 'dir' | 'image' | 'text' | 'binary'

const IMAGE_EXT = new Set([
  'png',
  'jpg',
  'jpeg',
  'gif',
  'webp',
  'svg',
  'avif',
  'bmp',
  'ico',
])

// 「能当文本读」的扩展名。刻意做成白名单而不是「不在图片里就算文本」：
// 白名单猜错的代价是多个「下载」按钮，黑名单猜错的代价是把 .zip 当文本
// 渲染成一屏乱码，还会白读一次 512KB。
const TEXT_EXT = new Set([
  'txt',
  'md',
  'markdown',
  'mdx',
  'rst',
  'json',
  'jsonl',
  'json5',
  'yaml',
  'yml',
  'toml',
  'ini',
  'cfg',
  'conf',
  'env',
  'csv',
  'tsv',
  'log',
  'sh',
  'bash',
  'zsh',
  'fish',
  'ps1',
  'py',
  'js',
  'mjs',
  'cjs',
  'ts',
  'tsx',
  'jsx',
  'go',
  'rs',
  'java',
  'kt',
  'kts',
  'c',
  'h',
  'cc',
  'cpp',
  'hpp',
  'cs',
  'rb',
  'php',
  'swift',
  'scala',
  'lua',
  'pl',
  'r',
  'sql',
  'css',
  'scss',
  'less',
  'html',
  'htm',
  'xml',
  'svg',
  'vue',
  'svelte',
  'gradle',
  'properties',
  'gitignore',
  'dockerignore',
  'patch',
  'diff',
])

// 没有扩展名但确定是文本的文件名（小写比较）。
const TEXT_NAMES = new Set([
  'dockerfile',
  'makefile',
  'license',
  'notice',
  'readme',
  'changelog',
  '.gitignore',
  '.dockerignore',
  '.env',
  '.editorconfig',
  '.npmrc',
])

/** 取小写扩展名（不含点）。没有扩展名返回空串。 */
export function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  if (i <= 0 || i === name.length - 1) return ''
  return name.slice(i + 1).toLowerCase()
}

/** 文件名主体的最后一段（同时认 / 和 \，agent 在 Windows 沙箱里也会产出这种）。 */
export function baseName(path: string): string {
  const cut = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'))
  return cut >= 0 ? path.slice(cut + 1) : path
}

export function isImageName(name: string): boolean {
  return IMAGE_EXT.has(extOf(baseName(name)))
}

export function isTextName(name: string): boolean {
  const base = baseName(name)
  const lower = base.toLowerCase()
  if (TEXT_NAMES.has(lower)) return true
  return TEXT_EXT.has(extOf(base))
}

export function classifyFile(name: string, isDir = false): FileKind {
  if (isDir) return 'dir'
  if (isImageName(name)) return 'image'
  if (isTextName(name)) return 'text'
  return 'binary'
}

/**
 * 带单位的字节数。
 *
 * 和 format.ts 的 humanSize 并存是有意的：那个是给「文件列表右侧的小字」用的，
 * 上限只到 MB；下载进度要显示 GB 级（模型权重、数据集），且 0 字节不该渲染成
 * 「0 B」以外的任何东西。
 */
export function formatSize(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—'
  if (n < 1024) return `${Math.round(n)} B`
  const kb = n / 1024
  if (kb < 1024) return `${kb.toFixed(1)} KB`
  const mb = kb / 1024
  if (mb < 1024) return `${mb.toFixed(1)} MB`
  return `${(mb / 1024).toFixed(2)} GB`
}

/**
 * 把后端/agent 给的名字洗成「能安全落盘」的名字。
 *
 * 三层考虑：
 *  1. 路径穿越——`../../.ssh/authorized_keys` 直接当文件名，某些下载器会真的
 *     按路径写；iOS 的分享面板也会把 `/` 当分隔符。
 *  2. 控制字符——\0 截断、\r\n 在 Content-Disposition 里是头注入。
 *  3. Windows 的保留字符与结尾点/空格（"CON"、"a." 这类落盘会失败）。
 *
 * 只取最后一段而不是直接拒绝：用户点的是「下载这个文件」，能下一个改名后的
 * 文件比弹一个错误有用。
 */
export function sanitizeFilename(raw: string, fallback = 'download'): string {
  if (!raw) return fallback
  let name = baseName(raw)
  // 控制字符与 Windows 非法字符统一替换成下划线；`..` 段直接丢掉。
  name = name
    .replace(/[\u0000-\u001f\u007f]/g, '')
    .replace(/[<>:"|?*]/g, '_')
    .replace(/\.{2,}/g, '.')
    .trim()
    // Windows 不允许以点或空格结尾（"a.txt." 会被静默截断）
    .replace(/[. ]+$/, '')
  if (!name || name === '.' || name === '..') return fallback
  if (/^[.]+$/.test(name)) return fallback
  // 绝大多数文件系统单个名字上限 255 字节，留余量；截断时保住扩展名。
  if (name.length > 180) {
    const ext = extOf(name)
    const keep = 180 - (ext ? ext.length + 1 : 0)
    name = `${name.slice(0, Math.max(1, keep))}${ext ? `.${ext}` : ''}`
  }
  return name
}

/** 从路径推出下载时用的文件名。 */
export function downloadFilename(path: string): string {
  return sanitizeFilename(baseName(path), 'download')
}

/**
 * 中间省略：长路径在 330px 的侧栏里只能横着滚，而横向滚动条比省略号更难用。
 * 保留首尾两段信息（根目录 + 文件名），中间吃掉。
 */
export function middleEllipsis(s: string, max = 28): string {
  if (!s) return ''
  if (s.length <= max) return s
  if (max <= 1) return '…'
  // 尾部多留一点：扩展名和文件名主体比中间的目录重要
  const tail = Math.ceil((max - 1) * 0.6)
  const head = max - 1 - tail
  return `${s.slice(0, head)}…${s.slice(s.length - tail)}`
}

/** 下载进度百分比；拿不到总长度时返回 null，UI 显示不确定态。 */
export function progressPercent(loaded: number, total: number | null | undefined): number | null {
  if (!total || total <= 0) return null
  const p = Math.round((loaded / total) * 100)
  return Math.min(100, Math.max(0, p))
}

/**
 * 超过这个大小就先问一句。
 *
 * 为什么是 50MB：后端现在是「整包读进内存再写盘」，一条 500MB 的产出物在手机上
 * 会先把 Safari 的标签页撑崩——那时用户看到的不是「下载失败」，是页面白屏。
 * 2MB 那个数量级（一次 agent 产出）不需要打断，直接下并显示进度即可。
 */
export const BIG_FILE_BYTES = 50 * 1024 * 1024

export function needsBigFileWarning(size: number): boolean {
  return size >= BIG_FILE_BYTES
}

/** 下载失败的可读文案。status=0 表示请求压根没发出去（网络层）。 */
export function describeDownloadError(status: number): string {
  if (status === 0) return '网络不通，文件没下下来，请检查连接后重试'
  if (status === 401) return '登录已过期，请重新登录后再下载'
  if (status === 403) return '无权访问这个文件'
  if (status === 404) return '文件不存在，可能已被删除或移动'
  if (status === 416) return '文件已变化，请刷新后重试'
  if (status >= 500) return `服务端出错（${status}），请稍后重试`
  return `下载失败（HTTP ${status}）`
}

/** 解析 Content-Disposition 里的文件名，优先 RFC 5987 的 filename*。 */
export function parseFilenameDisposition(header: string | null): string {
  if (!header) return ''
  const star = /filename\*\s*=\s*UTF-8''([^;]+)/i.exec(header)
  if (star) {
    try {
      return decodeURIComponent(star[1].trim())
    } catch {
      /* 编码坏了就退回 filename= */
    }
  }
  const plain = /filename\s*=\s*"([^"]*)"/i.exec(header) || /filename\s*=\s*([^;]+)/i.exec(header)
  if (!plain) return ''
  return plain[1].trim()
}

// ── 浏览器副作用 ────────────────────────────────────────────────

/**
 * 认证 token。
 *
 * 这里直接读 localStorage 的键，而不是从 auth.ts 取，是为了不让文件模块
 * 依赖整个认证状态机（那会把 useAuth 的订阅也拖进来）。键名与 auth.ts 的
 * TOKEN_KEY 必须一致——现成的单测锁不住这一点，所以注释留在这儿。
 */
export const TOKEN_STORAGE_KEY = 'astra_token'

function readToken(): string {
  try {
    return localStorage.getItem(TOKEN_STORAGE_KEY) || ''
  } catch {
    return ''
  }
}

export function downloadUrl(sessionId: string, path: string): string {
  return `/v1/sessions/${encodeURIComponent(sessionId)}/download?path=${encodeURIComponent(path)}`
}

export class DownloadError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'DownloadError'
    this.status = status
  }
}

export type FetchOptions = {
  token?: string
  signal?: AbortSignal
  onProgress?: (loaded: number, total: number | null) => void
  /** 建立连接的超时（拿不到响应头就放弃）。读数据阶段用 idleMs 单独管。 */
  connectTimeoutMs?: number
  /** 多久没有新数据算卡死。控制面在本地，30s 足够。 */
  idleTimeoutMs?: number
}

export type FetchedFile = { blob: Blob; filename: string; size: number }

/**
 * 取回文件字节。
 *
 * 用流式 reader 而不是 res.blob() 只为一件事：进度。res.blob() 在完成前
 * 拿不到任何中间状态，大文件会变成一个「点了没反应」的按钮。
 */
export async function fetchFileBlob(
  sessionId: string,
  path: string,
  opts: FetchOptions = {},
): Promise<FetchedFile> {
  const ctrl = new AbortController()
  let timedOut = false
  const timer = { id: 0 as number | undefined }
  const arm = (ms: number) => {
    if (timer.id) clearTimeout(timer.id)
    timer.id = setTimeout(() => {
      timedOut = true
      ctrl.abort()
    }, ms) as unknown as number
  }
  const disarm = () => {
    if (timer.id) clearTimeout(timer.id)
    timer.id = undefined
  }
  const onOuterAbort = () => ctrl.abort()
  opts.signal?.addEventListener('abort', onOuterAbort)

  const token = opts.token ?? readToken()
  arm(opts.connectTimeoutMs ?? 20000)

  try {
    let res: Response
    try {
      res = await fetch(downloadUrl(sessionId, path), {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
        signal: ctrl.signal,
        // 下载不需要缓存副本：workspace 里的同名文件随时可能被下一轮覆盖
        cache: 'no-store',
      })
    } catch {
      throw new DownloadError(0, describeDownloadError(0))
    }

    if (!res.ok) {
      throw new DownloadError(res.status, describeDownloadError(res.status))
    }

    const totalHeader = Number(res.headers.get('Content-Length') || '')
    const total = Number.isFinite(totalHeader) && totalHeader > 0 ? totalHeader : null
    // 优先用服务端给的名字（它已经做过头注入清洗），拿不到才从路径推
    const filename =
      sanitizeFilename(parseFilenameDisposition(res.headers.get('Content-Disposition')), '') ||
      downloadFilename(path)

    const chunks: BlobPart[] = []
    let loaded = 0
    opts.onProgress?.(0, total)

    if (res.body) {
      const reader = res.body.getReader()
      for (;;) {
        arm(opts.idleTimeoutMs ?? 30000)
        const { value, done } = await reader.read()
        if (done) break
        if (value) {
          chunks.push(value as unknown as BlobPart)
          loaded += value.byteLength
          opts.onProgress?.(loaded, total)
        }
      }
    } else {
      // 老浏览器没有 response.body（或响应被代理降级），退回一次性读取
      const buf = await res.arrayBuffer()
      chunks.push(buf)
      loaded = buf.byteLength
      opts.onProgress?.(loaded, total)
    }
    disarm()

    const type = res.headers.get('Content-Type') || 'application/octet-stream'
    const blob = new Blob(chunks, { type: type.split(';')[0] || 'application/octet-stream' })
    return { blob, filename, size: blob.size }
  } catch (e) {
    if (e instanceof DownloadError) throw e
    if (timedOut) throw new DownloadError(0, '下载超时，文件可能较大或网络不稳定')
    if ((e as Error)?.name === 'AbortError') throw new DownloadError(0, '已取消下载')
    throw new DownloadError(0, describeDownloadError(0))
  } finally {
    disarm()
    opts.signal?.removeEventListener('abort', onOuterAbort)
  }
}

/** 触发浏览器保存。URL 必须撤销：一个 200MB 的 blob 不撤销就跟着页面活到刷新。 */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = sanitizeFilename(filename)
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  a.remove()
  // 立刻 revoke 会让部分浏览器（Safari）来不及读，延后一拍
  setTimeout(() => URL.revokeObjectURL(url), 10000)
}

type ShareNavigator = {
  canShare?: (data?: { files?: File[] }) => boolean
  share?: (data: { files?: File[]; title?: string }) => Promise<void>
}

/** 纯判断：这个 navigator 能不能分享带文件的载荷。 */
export function canShareFiles(nav: ShareNavigator | undefined, files: File[]): boolean {
  if (!nav || typeof nav.share !== 'function' || typeof nav.canShare !== 'function') return false
  try {
    return nav.canShare({ files }) === true
  } catch {
    return false
  }
}

/**
 * 面板要不要显示「分享」。
 *
 * 用一个 1 字节的假文件探：canShare 的判定跟 MIME/数量有关，传空数组在部分
 * 安卓 WebView 上恒为 false，探不出真实能力。不支持就隐藏按钮，而不是给一个
 * 灰的点不动的——手机上灰按钮只会让人以为界面坏了。
 */
export function supportsFileShare(nav: ShareNavigator | undefined = globalThis.navigator as ShareNavigator): boolean {
  if (typeof File !== 'function') return false
  try {
    const probe = new File([new Uint8Array([1])], 'probe.txt', { type: 'text/plain' })
    return canShareFiles(nav, [probe])
  } catch {
    return false
  }
}

/** 系统分享面板。用户取消（AbortError）不算失败。 */
export async function shareBlob(blob: Blob, filename: string): Promise<'shared' | 'cancelled'> {
  const file = new File([blob], sanitizeFilename(filename), {
    type: blob.type || 'application/octet-stream',
  })
  try {
    await navigator.share({ files: [file], title: file.name })
    return 'shared'
  } catch (e) {
    if ((e as Error)?.name === 'AbortError') return 'cancelled'
    throw e
  }
}

/**
 * 把路径写进 composer 的输入框。
 *
 * 为什么不通过 props：Composer 的文本是它自己的 useState，没有受控入口，
 * 而这一轮不允许改那个文件。走 DOM + 原生 setter 是唯一不越界的办法：
 * 直接 `el.value = x` 会被 React 的 value tracker 当成「没变化」而不触发 onChange，
 * 必须先调原型上的 setter 再派发 input 事件。
 *
 * 同时广播一个自定义事件：等 Composer 自己愿意接的时候，删掉上面这段即可。
 */
export function insertIntoComposer(text: string): boolean {
  const el = document.querySelector<HTMLTextAreaElement>('.composer textarea')
  if (!el) return false
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set
  const prev = el.value
  const next = prev && !/\s$/.test(prev) ? `${prev} ${text}` : `${prev}${text}`
  if (setter) setter.call(el, next)
  else el.value = next
  el.dispatchEvent(new Event('input', { bubbles: true }))
  el.focus()
  try {
    el.setSelectionRange(next.length, next.length)
  } catch {
    /* 某些浏览器对隐藏元素抛错，无所谓 */
  }
  window.dispatchEvent(new CustomEvent('astra:composer-insert', { detail: { text } }))
  return true
}
