// 自闭环三个页面共用的展示件：503 未启用引导 + 加载骨架。
//
// 复用现有 .card / .card-body / .notice / .settings-kv / .sk-stack 等类，
// 只补一个 .loop-guide 修饰类（新增选择器统一 .loop- 前缀）。
// 颜色全部来自现有 CSS 变量，不硬编码深色底。

import { Notice } from '../../components/ui'
import { GUIDES } from './loopModel'
import type { LoopPage } from './loopModel'

/** 能力未启用（503 等「拿不到清单」状态）时的开箱指引，不给空图表。 */
export function DisabledGuide({ page, onRetry }: { page: LoopPage; onRetry: () => void }) {
  const g = GUIDES[page]
  return (
    <div className="card loop-guide">
      <div className="card-head">{g.title}</div>
      <div className="card-body">
        <Notice kind="warn">
          {g.intro}
          <div className="notice-sub">
            服务端返回 503（或等价的「能力不可用」状态），说明这页对应的能力当前未启用。
            <button className="btn small" onClick={onRetry}>
              重试
            </button>
          </div>
        </Notice>

        <div className="settings-kv">
          {g.rows.map((r) => (
            <div className="settings-kv-row" key={r.k}>
              <div className="settings-kv-key mono">{r.k}</div>
              <div className="settings-kv-val dim small">{r.v}</div>
            </div>
          ))}
        </div>

        <div className="settings-notes">
          <p>{g.note}</p>
        </div>
      </div>
    </div>
  )
}

/** 加载骨架：沿用 .skeleton 的动效，这里只负责排布。 */
export function Skel({ rows = 4 }: { rows?: number }) {
  return (
    <div className="sk-stack" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <div
          key={i}
          className="skeleton"
          style={{ height: 12, width: `${92 - (i % 3) * 14}%` }}
        />
      ))}
    </div>
  )
}
