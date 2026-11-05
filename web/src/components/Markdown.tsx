import React from 'react'

// 一个够用就好的 Markdown 渲染器。
//
// 为什么不上 react-markdown：它带 40 多个传递依赖，而这里只需要
// 代码块 / 标题 / 列表 / 行内代码 / 粗体 / 链接六种语法。
// agent 的输出里 90% 是这几样，剩下 10% 走「原样当文本」也不会误导人。
// 更重要的是：不渲染 HTML，天然没有 XSS 面。

type Block =
  | { t: 'p'; lines: string[] }
  | { t: 'code'; lang: string; code: string }
  | { t: 'h'; level: number; text: string }
  | { t: 'ul' | 'ol'; items: string[] }
  | { t: 'quote'; lines: string[] }

const isFence = (l: string) => /^\s*```/.test(l)
const isHeading = (l: string) => /^#{1,4}\s+/.test(l)
const isUl = (l: string) => /^\s*[-*+]\s+/.test(l)
const isOl = (l: string) => /^\s*\d+\.\s+/.test(l)
const isQuote = (l: string) => /^\s*>\s?/.test(l)
const isBlockStart = (l: string) =>
  isFence(l) || isHeading(l) || isUl(l) || isOl(l) || isQuote(l)

function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n')
  const out: Block[] = []
  let i = 0

  while (i < lines.length) {
    const line = lines[i]

    if (isFence(line)) {
      const lang = line.trim().slice(3).trim()
      const buf: string[] = []
      i++
      while (i < lines.length && !isFence(lines[i])) buf.push(lines[i++])
      i++ // 跳过收尾的 ```
      out.push({ t: 'code', lang, code: buf.join('\n') })
      continue
    }

    if (isHeading(line)) {
      const m = line.match(/^(#{1,4})\s+(.*)$/)!
      out.push({ t: 'h', level: m[1].length, text: m[2] })
      i++
      continue
    }

    if (isUl(line) || isOl(line)) {
      const ordered = isOl(line)
      const items: string[] = []
      while (i < lines.length && (ordered ? isOl(lines[i]) : isUl(lines[i]))) {
        items.push(lines[i].replace(ordered ? /^\s*\d+\.\s+/ : /^\s*[-*+]\s+/, ''))
        i++
      }
      out.push({ t: ordered ? 'ol' : 'ul', items })
      continue
    }

    if (isQuote(line)) {
      const buf: string[] = []
      while (i < lines.length && isQuote(lines[i])) {
        buf.push(lines[i].replace(/^\s*>\s?/, ''))
        i++
      }
      out.push({ t: 'quote', lines: buf })
      continue
    }

    if (line.trim() === '') {
      i++
      continue
    }

    const buf = [line]
    i++
    while (i < lines.length && lines[i].trim() !== '' && !isBlockStart(lines[i])) {
      buf.push(lines[i++])
    }
    out.push({ t: 'p', lines: buf })
  }

  return out
}

const INLINE = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|\[[^\]\n]+\]\([^)\s]+\))/g

function renderInline(text: string, keyPrefix: string): React.ReactNode[] {
  const parts = text.split(INLINE)
  return parts.map((part, idx) => {
    const key = `${keyPrefix}-${idx}`
    if (!part) return null
    if (part.startsWith('`') && part.endsWith('`')) {
      return <code key={key}>{part.slice(1, -1)}</code>
    }
    if (part.startsWith('**') && part.endsWith('**')) {
      return <strong key={key}>{part.slice(2, -2)}</strong>
    }
    if (part.startsWith('*') && part.endsWith('*') && part.length > 2) {
      return <em key={key}>{part.slice(1, -1)}</em>
    }
    const link = part.match(/^\[([^\]]+)\]\(([^)\s]+)\)$/)
    if (link) {
      return (
        <a key={key} href={link[2]} target="_blank" rel="noreferrer noopener">
          {link[1]}
        </a>
      )
    }
    return <React.Fragment key={key}>{part}</React.Fragment>
  })
}

export function Markdown({ text }: { text: string }) {
  const blocks = React.useMemo(() => parseBlocks(text), [text])
  return (
    <div className="md">
      {blocks.map((b, i) => {
        switch (b.t) {
          case 'code':
            return (
              <pre key={i}>
                <code>{b.code}</code>
              </pre>
            )
          case 'h': {
            const Tag = (`h${Math.min(b.level, 3)}`) as 'h1' | 'h2' | 'h3'
            return <Tag key={i}>{renderInline(b.text, `h${i}`)}</Tag>
          }
          case 'ul':
            return (
              <ul key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{renderInline(it, `u${i}${j}`)}</li>
                ))}
              </ul>
            )
          case 'ol':
            return (
              <ol key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{renderInline(it, `o${i}${j}`)}</li>
                ))}
              </ol>
            )
          case 'quote':
            return (
              <blockquote key={i}>
                {b.lines.map((l, j) => (
                  <div key={j}>{renderInline(l, `q${i}${j}`)}</div>
                ))}
              </blockquote>
            )
          default:
            return (
              <p key={i}>
                {b.lines.map((l, j) => (
                  <React.Fragment key={j}>
                    {j > 0 && <br />}
                    {renderInline(l, `p${i}${j}`)}
                  </React.Fragment>
                ))}
              </p>
            )
        }
      })}
    </div>
  )
}
