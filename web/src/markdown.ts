// 极简 Markdown 渲染器（无依赖，输出安全 HTML 字符串）。
//
// 为什么自己写：整个前端只有两处需要 markdown（对话气泡与文档页）。引一个
// 解析库再加一个 sanitizer，体积比整个应用的业务代码还大，而文档里实际用到的
// 语法只有标题 / 段落 / 列表 / 代码块 / 表格 / 引用这六种。
//
// ── 安全模型（只有一条规则，但必须严格守住）──────────────────────
//   1. **先整体 HTML 转义，再按白名单拼接标签。** 任何来自输入的字
//      符在进入输出前都已经变成实体（&lt; &gt; &amp; &quot; &#39;），
//      输出的标签全部由本文件硬编码，不存在「把原文当 HTML 输出」的分支。
//   2. 行内代码 / 链接先抽成占位符再从已转义的文本里拼回去，占位符用
//      U+0000 / U+0001，这两个字符不可能由输入直接产生标签。
//   3. 链接 href 走协议白名单（http(s)://、/、#），javascript: / data: /
//      vbscript: 一律丢弃 href，只保留链接文字。
//   4. 不做图片：`![alt](url)` 渲染成一个普通链接。文档里几乎没有图片，
//      而 <img src> 会多出一条注入面（远程加载、onerror 之类）。
//
// 不做的事：语法高亮、脚注、任务列表、HTML 透传。需要它们时再引库，
// 不要在这里堆 regex。

const ESCAPES: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
}

/** HTML 转义。导出给页面复用（比如把标题塞进非 React 的字符串场景）。 */
export function escapeHtml(text: string): string {
  return String(text ?? '').replace(/[&<>"']/g, (ch) => ESCAPES[ch] ?? ch)
}

/**
 * href 白名单：只允许 http(s)://、站内绝对路径 `/`、页内锚点 `#`。
 *
 * 判定前先去掉控制字符与空白，`java\nscript:` 这种拆分写法也就过不了。
 * 入参可能已经 HTML 转义过（`&` 变成 `&amp;`），这不影响前缀判断——
 * 会被转义的都是 `&`/引号，协议名本身是字母。
 */
export function isSafeHref(url: string): boolean {
  const u = String(url || '').trim()
  if (!u) return false
  const compact = u.replace(/[\u0000-\u0020\u007f]/g, '').toLowerCase()
  if (compact.startsWith('http://') || compact.startsWith('https://')) return true
  return u.startsWith('/') || u.startsWith('#')
}

/** 粗体：`**x**` → `<strong>`。入参必须已转义。 */
function strong(escaped: string): string {
  return escaped.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
}

/**
 * 行内格式。入参是**原始**（未转义）的一行文字。
 *
 * 顺序：行内代码 → 整体转义 → 图片 → 链接 → 粗体 → 还原占位符。
 * 代码先抽走，里面的 `**` / `[]()` 就不会被后续规则改写；链接先抽走，
 * URL 里的 `*` 也不会被粗体规则打断。
 */
function inline(raw: string): string {
  // 占位符用的是 U+0000 / U+0001，先把输入里的这两个字符去掉，
  // 保证「用户文字」永远不可能被当成占位符还原。
  const input = raw.replace(/[\u0000\u0001]/g, '')
  const codes: string[] = []
  let s = input.replace(/`([^`\n]+)`/g, (_m, code: string) => {
    codes.push(`<code>${escapeHtml(code.trim())}</code>`)
    return `\u0000${codes.length - 1}\u0000`
  })

  s = escapeHtml(s)

  const held: string[] = []
  const hold = (chunk: string) => `\u0001${held.push(chunk) - 1}\u0001`

  // 图片：不输出 <img>，按链接渲染。alt 为空时用 URL 当文字。
  s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (_m, alt: string, url: string) => {
    if (!isSafeHref(url)) return alt
    const text = alt.trim() || url
    const ext = /^https?:\/\//i.test(url)
    const attrs = ext ? ' target="_blank" rel="noreferrer noopener"' : ''
    return hold(`<a href="${url}"${attrs}>${text}</a>`)
  })

  s = s.replace(/\[([^\]]*)\]\(([^)\s]+)\)/g, (_m, text: string, url: string) => {
    // 协议不在白名单：丢掉链接，保留文字。比整段删掉更不容易丢信息，
    // 也不会把 `javascript:` 留在 href 里。
    if (!isSafeHref(url)) return text
    const ext = /^https?:\/\//i.test(url)
    const attrs = ext ? ' target="_blank" rel="noreferrer noopener"' : ''
    return hold(`<a href="${url}"${attrs}>${strong(text) || url}</a>`)
  })

  s = strong(s)

  s = s.replace(/\u0001(\d+)\u0001/g, (_m, i: string) => held[Number(i)] ?? '')
  s = s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => codes[Number(i)] ?? '')
  return s
}

// ── 行级判定 ──────────────────────────────────────────────────

/** CJK 与全角标点。判断「折行的中文要不要补空格」。 */
const CJK = /[\u2e80-\u9fff\u3000-\u303f\uff01-\uff60]/

/**
 * 合并段落内的连续行。
 *
 * 中文文档常按屏宽手工折行，直接 join(' ') 会在句子中间留一个空格；
 * 两侧都是中日韩字符时不补空格，其余情况（英文单词）补一个。
 */
function joinSoft(lines: string[]): string {
  let out = ''
  for (const line of lines) {
    const t = line.trim()
    if (!t) continue
    if (!out) {
      out = t
      continue
    }
    const glue = CJK.test(out.slice(-1)) && CJK.test(t.slice(0, 1)) ? '' : ' '
    out += glue + t
  }
  return out
}

function splitRow(line: string): string[] {
  let t = line.trim()
  if (t.startsWith('|')) t = t.slice(1)
  if (t.endsWith('|')) t = t.slice(0, -1)
  return t.split('|').map((c) => c.trim())
}

/** 表格分隔行：`|---|:--:|`。每格只允许冒号与短横。 */
function isTableSep(line: string): boolean {
  if (!line.includes('-')) return false
  const cells = splitRow(line)
  return cells.length > 0 && cells.every((c) => /^:?-{1,}:?$/.test(c))
}

const RE_HEADING = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/
const RE_HR = /^\s{0,3}(-{3,}|\*{3,}|_{3,})\s*$/
const RE_QUOTE = /^\s{0,3}>/
const RE_LIST = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/
const RE_FENCE = /^\s{0,3}(`{3,}|~{3,})\s*([^`]*)$/

/** 这一行会不会开启一个新块（段落收集时遇到就该停）。 */
function startsBlock(line: string, next?: string): boolean {
  return (
    RE_HEADING.test(line) ||
    RE_FENCE.test(line) ||
    RE_QUOTE.test(line) ||
    RE_LIST.test(line) ||
    RE_HR.test(line) ||
    (line.includes('|') && next !== undefined && isTableSep(next))
  )
}

// ── 列表 ──────────────────────────────────────────────────────

type ListItem = { level: 1 | 2; ordered: boolean; text: string }

/**
 * 两级列表。二级嵌在上一项的 `<li>` 里面（合法嵌套），不同列表类型
 * （`-` 与 `1.`）切换时重新开一个列表。
 *
 * 兜底：如果第一条就是二级（没有父项），降级成一级——否则外层循环
 * 会一直看到「二级但没人消费」而无法前进。
 */
function renderList(items: ListItem[]): string {
  const list = items.map((it, n) =>
    n === 0 && it.level === 2 ? { ...it, level: 1 as const } : it,
  )
  let out = ''
  let idx = 0
  while (idx < list.length) {
    const ordered = list[idx].ordered
    const tag = ordered ? 'ol' : 'ul'
    out += `<${tag}>`
    while (idx < list.length) {
      const it = list[idx]
      if (it.level === 2 || it.ordered !== ordered) break
      out += `<li>${inline(it.text)}`
      idx++
      if (idx < list.length && list[idx].level === 2) {
        const subTag = list[idx].ordered ? 'ol' : 'ul'
        out += `<${subTag}>`
        while (idx < list.length && list[idx].level === 2) {
          out += `<li>${inline(list[idx].text)}</li>`
          idx++
        }
        out += `</${subTag}>`
      }
      out += '</li>'
    }
    out += `</${tag}>`
    // 孤立的二级（父项被上一条不同 ordered 的列表吃掉）：降级，保证前进。
    if (idx < list.length && list[idx].level === 2) {
      list[idx] = { ...list[idx], level: 1 }
    }
  }
  return out
}

// ── 块级渲染 ──────────────────────────────────────────────────

function renderBlocks(md: string, depth: number): string {
  const lines = md.split('\n')
  const out: string[] = []
  let i = 0

  while (i < lines.length) {
    const line = lines[i]
    if (!line.trim()) {
      i++
      continue
    }

    // 围栏代码块（未闭合也吃掉剩下的全部内容，不让它变成一堆段落）
    const fence = RE_FENCE.exec(line)
    if (fence) {
      const mark = fence[1][0]
      // 只取 info string 的第一个词当语言标识，字符集也收紧：
      // class 属性里不允许出现空格/引号，直接从源头滤掉。
      const lang = (fence[2].trim().split(/\s+/)[0] || '')
        .toLowerCase()
        .replace(/[^a-z0-9#+._-]/g, '')
      const close = mark === '`' ? /^\s{0,3}`{3,}\s*$/ : /^\s{0,3}~{3,}\s*$/
      const buf: string[] = []
      i++
      while (i < lines.length && !close.test(lines[i])) {
        buf.push(lines[i])
        i++
      }
      if (i < lines.length) i++ // 吃掉收尾围栏
      const cls = lang ? ` class="language-${lang}"` : ''
      out.push(`<pre><code${cls}>${escapeHtml(buf.join('\n'))}</code></pre>`)
      continue
    }

    const heading = RE_HEADING.exec(line)
    if (heading) {
      // 只支持到 h4：文档正文的层级再深就该拆文件了，深标题也读不出来。
      const level = Math.min(heading[1].length, 4)
      out.push(`<h${level}>${inline(heading[2])}</h${level}>`)
      i++
      continue
    }

    if (RE_HR.test(line)) {
      out.push('<hr />')
      i++
      continue
    }

    if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = splitRow(line)
      i += 2
      const rows: string[][] = []
      while (i < lines.length && lines[i].trim() && lines[i].includes('|')) {
        rows.push(splitRow(lines[i]))
        i++
      }
      let html = '<div class="docs-table"><table><thead><tr>'
      html += head.map((c) => `<th>${inline(c)}</th>`).join('')
      html += '</tr></thead><tbody>'
      for (const row of rows) {
        html += '<tr>'
        for (let c = 0; c < head.length; c++) {
          html += `<td>${inline(row[c] ?? '')}</td>`
        }
        html += '</tr>'
      }
      html += '</tbody></table></div>'
      out.push(html)
      continue
    }

    if (RE_QUOTE.test(line)) {
      const buf: string[] = []
      while (i < lines.length && RE_QUOTE.test(lines[i])) {
        buf.push(lines[i].replace(/^\s{0,3}>\s?/, ''))
        i++
      }
      const inner = depth < 3 ? renderBlocks(buf.join('\n'), depth + 1) : `<p>${inline(joinSoft(buf))}</p>`
      out.push(`<blockquote>${inner}</blockquote>`)
      continue
    }

    const listStart = RE_LIST.exec(line)
    if (listStart) {
      const items: ListItem[] = []
      while (i < lines.length) {
        const m = RE_LIST.exec(lines[i])
        if (!m) break
        const indent = m[1].replace(/\t/g, '  ').length
        let text = m[3]
        i++
        // 缩进续行并入当前项（列表项里的长句折行）。
        while (
          i < lines.length &&
          lines[i].trim() !== '' &&
          !RE_LIST.test(lines[i]) &&
          /^\s{2,}/.test(lines[i]) &&
          !startsBlock(lines[i], lines[i + 1])
        ) {
          text += ' ' + lines[i].trim()
          i++
        }
        items.push({ level: indent >= 2 ? 2 : 1, ordered: /\d/.test(m[2][0]), text })
      }
      out.push(renderList(items))
      continue
    }

    // 段落：收集到空行或下一个块起始为止
    const buf: string[] = []
    while (i < lines.length && lines[i].trim() && !startsBlock(lines[i], lines[i + 1])) {
      buf.push(lines[i])
      i++
    }
    // 兜底：如果第一行就命中了某个块规则却被前面所有分支漏掉，至少前进一行，
    // 避免死循环（新增块类型时忘记在 startsBlock 里登记会命中这里）。
    if (buf.length === 0) {
      buf.push(lines[i])
      i++
    }
    out.push(`<p>${inline(joinSoft(buf))}</p>`)
  }

  return out.join('\n')
}

/**
 * 把 Markdown 渲染成 HTML 字符串。返回值可以直接交给
 * `dangerouslySetInnerHTML`——所有来自输入的内容都已被转义。
 */
export function renderMarkdown(md: string): string {
  if (!md) return ''
  const text = String(md).replace(/^\uFEFF/, '').replace(/\r\n?/g, '\n')
  if (!text.trim()) return ''
  return renderBlocks(text, 0)
}
