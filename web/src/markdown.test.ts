import { describe, expect, it } from 'vitest'
import { escapeHtml, isSafeHref, renderMarkdown } from './markdown'

// 断言尽量落在「片段」上：整串比对会让加一个空行就红一片，
// 而这些用例真正要盯的是「转义了没 / 标签对不对 / 危险协议挡没挡」。

describe('escapeHtml', () => {
  it('把五个 HTML 元字符全部转成实体', () => {
    expect(escapeHtml(`<a href="x">'&'</a>`)).toBe(
      '&lt;a href=&quot;x&quot;&gt;&#39;&amp;&#39;&lt;/a&gt;',
    )
    expect(escapeHtml('')).toBe('')
  })
})

describe('标题', () => {
  it('# ~ #### 各生成对应层级，>4 级收敛到 h4', () => {
    const html = renderMarkdown('# 一\n## 二\n### 三\n#### 四\n##### 五')
    expect(html).toContain('<h1>一</h1>')
    expect(html).toContain('<h2>二</h2>')
    expect(html).toContain('<h3>三</h3>')
    expect(html).toContain('<h4>四</h4>')
    expect(html).toContain('<h4>五</h4>')
    expect(html).not.toContain('<h5>')
  })
})

describe('段落', () => {
  it('连续行合并；中文折行不补空格，英文之间补一个', () => {
    expect(renderMarkdown('第一行\n第二行')).toBe('<p>第一行第二行</p>')
    expect(renderMarkdown('hello\nworld')).toBe('<p>hello world</p>')
  })

  it('空行分段', () => {
    expect(renderMarkdown('一\n\n二')).toBe('<p>一</p>\n<p>二</p>')
  })
})

describe('列表', () => {
  it('无序列表 + 两级缩进', () => {
    const html = renderMarkdown('- 一\n  - 一之一\n- 二')
    expect(html).toBe('<ul><li>一<ul><li>一之一</li></ul></li><li>二</li></ul>')
  })

  it('有序列表与无序列表各自成表，切换时重新开一个', () => {
    expect(renderMarkdown('1. 甲\n2. 乙')).toBe('<ol><li>甲</li><li>乙</li></ol>')
    const mixed = renderMarkdown('- 甲\n1. 乙')
    expect(mixed).toBe('<ul><li>甲</li></ul><ol><li>乙</li></ol>')
  })
})

describe('代码块', () => {
  it('带语言标识，内部内容逐字转义且不解析 markdown', () => {
    const html = renderMarkdown('```go\nfmt.Println("<b>", **x**)\n```')
    expect(html).toContain('<pre><code class="language-go">')
    expect(html).toContain('&lt;b&gt;')
    expect(html).toContain('**x**')
    expect(html).not.toContain('<b>')
    expect(html.endsWith('</code></pre>')).toBe(true)
  })

  it('未闭合围栏也收口，剩余内容全部当代码', () => {
    const html = renderMarkdown('```\n未闭合\n# 这不是标题')
    expect(html.startsWith('<pre><code>')).toBe(true)
    expect(html).toContain('未闭合')
    expect(html).toContain('# 这不是标题')
    expect(html.endsWith('</code></pre>')).toBe(true)
    expect(html).not.toContain('<h1>')
  })

  it('语言标识不能注入属性（引号/尖括号被滤掉）', () => {
    const html = renderMarkdown('```"><script>alert(1)</script>\nx\n```')
    expect(html).not.toContain('"><')
    expect(html).not.toContain('<script>')
    // class 值只可能由 [a-z0-9#+._-] 组成——注入的引号/尖括号被滤掉。
    expect(html).toMatch(/class="language-[a-z0-9#+._-]*"/)
  })

  it('行内代码优先于粗体与链接规则', () => {
    const html = renderMarkdown('用 `**a**` 和 `[x](javascript:1)` 举例')
    expect(html).toContain('<code>**a**</code>')
    expect(html).toContain('<code>[x](javascript:1)</code>')
    expect(html).not.toContain('<strong>')
  })
})

describe('表格', () => {
  it('表头 + 分隔行 + 数据行，单元格支持行内格式', () => {
    const html = renderMarkdown('| 名称 | 说明 |\n|---|---|\n| a | `x` |\n| b | **粗** |')
    expect(html).toContain('<div class="docs-table"><table>')
    expect(html).toContain('<th>名称</th>')
    expect(html).toContain('<td>a</td><td><code>x</code></td>')
    expect(html).toContain('<td><strong>粗</strong></td>')
    expect(html).toContain('</tbody></table></div>')
  })

  it('缺列的表格行用空单元格补齐，不抛错', () => {
    expect(renderMarkdown('| a | b |\n|---|---|\n| 1 |')).toContain('<td>1</td><td></td>')
  })
})

describe('引用与水平线', () => {
  it('引用块包一层 blockquote，内部仍按块渲染', () => {
    const html = renderMarkdown('> 一句引用\n> 继续')
    expect(html).toContain('<blockquote>')
    expect(html).toContain('一句引用')
    expect(html).toContain('</blockquote>')
  })

  it('--- / *** / ___ 都是水平线', () => {
    expect(renderMarkdown('---')).toBe('<hr />')
    expect(renderMarkdown('***')).toBe('<hr />')
    expect(renderMarkdown('___')).toBe('<hr />')
  })
})

describe('链接白名单', () => {
  it('http / https / 站内路径 / 锚点放行，外链加 rel', () => {
    const html = renderMarkdown(
      '[a](https://example.com/x) [b](/admin/docs) [c](#top) [d](http://h)',
    )
    expect(html).toContain('<a href="https://example.com/x" target="_blank" rel="noreferrer noopener">a</a>')
    expect(html).toContain('<a href="/admin/docs">b</a>')
    expect(html).toContain('<a href="#top">c</a>')
    expect(html).toContain('<a href="http://h"')
    expect(isSafeHref('https://x')).toBe(true)
    expect(isSafeHref('/v1/docs')).toBe(true)
    expect(isSafeHref('#a')).toBe(true)
  })

  it('javascript: / data: / vbscript: / 相对裸域名一律丢掉 href，只留文字', () => {
    const html = renderMarkdown(
      '[x](javascript:alert(1)) [y](data:text/html;base64,PHN2Zz4=) [z](vbscript:msgbox) [w](example.com)',
    )
    expect(html).not.toContain('<a ')
    expect(html).not.toContain('javascript:')
    expect(html).toContain('x')
    expect(html).toContain('y')
    expect(html).toContain('w')
    expect(isSafeHref('javascript:alert(1)')).toBe(false)
    expect(isSafeHref(' java\nscript:alert(1)')).toBe(false)
    expect(isSafeHref('data:text/html,x')).toBe(false)
  })

  it('图片渲染成链接而不是 <img>', () => {
    const html = renderMarkdown('![示意图](/img/a.png)')
    expect(html).not.toContain('<img')
    expect(html).toContain('<a href="/img/a.png">示意图</a>')
  })
})

describe('XSS', () => {
  it('原始 HTML 标签被转义成文本，不会出现在输出标签里', () => {
    const html = renderMarkdown('<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>')
    expect(html).not.toContain('<script>')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;script&gt;')
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;')
  })

  it('粗体里的标签同样被转义', () => {
    expect(renderMarkdown('**<b>x</b>**')).toBe('<p><strong>&lt;b&gt;x&lt;/b&gt;</strong></p>')
  })

  it('链接 URL 里的引号被转成实体，无法闭合 href 属性', () => {
    const html = renderMarkdown('[x](https://a.com/"onmouseover="alert(1))')
    expect(html).not.toContain('"onmouseover="')
    expect(html).toContain('&quot;onmouseover=&quot;')
  })

  it('表格单元格与标题里的 HTML 也被转义', () => {
    const html = renderMarkdown('# <img src=x>\n\n| <script> | b |\n|---|---|\n| <i> | c |')
    expect(html).not.toContain('<img')
    expect(html).not.toContain('<script>')
    expect(html).toContain('&lt;img src=x&gt;')
  })
})

describe('空输入', () => {
  it('空串 / 纯空白 / 纯换行都返回空字符串', () => {
    expect(renderMarkdown('')).toBe('')
    expect(renderMarkdown('   \n\t\n')).toBe('')
    expect(renderMarkdown('\n\n')).toBe('')
  })
})
