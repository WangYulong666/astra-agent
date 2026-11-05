export function relTime(iso: string): string {
  if (!iso) return ''
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  const min = Math.floor(diff / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 7) return `${d} 天前`
  return new Date(t).toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' })
}

export function clockTime(iso: string): string {
  const t = new Date(iso)
  if (Number.isNaN(t.getTime())) return ''
  return t.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
}

export function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

export function shortId(id: string): string {
  return id.length > 10 ? `${id.slice(0, 6)}…${id.slice(-4)}` : id
}

/**
 * Go 的 time.Time 零值经 JSON 出来是 "0001-01-01T00:00:00Z"。
 * `json:"...,omitempty"` 对结构体不生效，所以「从未登录」「从未使用」
 * 这类字段拿到的是零值字符串而不是缺字段——必须先判掉再展示。
 */
export function isZeroTime(iso?: string): boolean {
  if (!iso) return true
  const y = Number(iso.slice(0, 4))
  return !Number.isFinite(y) || y < 1971
}

export function fmtTime(iso?: string): string {
  if (isZeroTime(iso)) return '—'
  const t = new Date(iso as string)
  if (Number.isNaN(t.getTime())) return '—'
  return t.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

const NUM = new Intl.NumberFormat('zh-CN')

/** 千分位整数。用量动辄几十万，不分组基本读不出量级。 */
export function fmtNumber(n: number | undefined): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '0'
  return NUM.format(n)
}

/**
 * 金额。库里存整数微美元，这里只在展示层换算（避免浮点累加误差）。
 * 保留 4 位：单次调用常在 $0.0001 量级，2 位会让整页都是 0.00。
 */
export function fmtUsd(microUsd: number | undefined): string {
  const v = (microUsd ?? 0) / 1e6
  return `$${v.toFixed(4)}`
}

