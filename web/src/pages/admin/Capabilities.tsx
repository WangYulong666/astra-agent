// 能力注册表（/admin/capabilities）。
//
// 单一事实来源：清单来自 GET /v1/capabilities，汇总 openapi 路由、两个 CLI 命令表、
// skills 目录与 /v1/meta 能力开关，不手抄。渐进式披露三层：
//   第一层（默认）name/kind/description/enabled/source；
//   第二层（?detail=full）endpoint / requiredEnv / requires / unavailable；
//   第三层（点 skill 条目）GET /v1/capabilities/{name} 拿 SKILL.md 全文。
//
// 筛选（kind / 只看已启用 / 关键字）在前端做，不额外打接口。

import { useCallback, useEffect, useState } from 'react'
import { capabilityApi } from '../../api'
import type { Capability } from '../../api'
import { Empty, Notice } from '../../components/ui'
import {
  countCapabilities,
  emptyText,
  filterCapabilities,
  isUnavailable,
  kindLabel,
} from './loopModel'
import type { CapabilityFilter } from './loopModel'
import { DisabledGuide, Skel } from './LoopGuide'

type Panel = { name: string; body: string; loading: boolean; err: string } | null

const KINDS: { value: CapabilityFilter['kind']; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'api', label: 'API' },
  { value: 'cli', label: 'CLI' },
  { value: 'skill', label: 'Skill' },
]

export function Capabilities() {
  const [items, setItems] = useState<Capability[] | null>(null)
  const [err, setErr] = useState('')
  const [disabled, setDisabled] = useState(false)
  const [full, setFull] = useState(false)

  const [kind, setKind] = useState<CapabilityFilter['kind']>('all')
  const [onlyEnabled, setOnlyEnabled] = useState(false)
  const [query, setQuery] = useState('')

  const [panel, setPanel] = useState<Panel>(null)

  const load = useCallback(async (detail: boolean) => {
    setErr('')
    setDisabled(false)
    setItems(null)
    setPanel(null)
    try {
      const res = await capabilityApi.list(detail)
      setItems(res.items || [])
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load(full)
    // full 变化时重新请求（切到 ?detail=full）。其余筛选只在前端做。
  }, [full, load])

  const toggleFull = () => setFull((v) => !v)

  const openSkill = async (name: string) => {
    if (panel?.name === name) {
      setPanel(null)
      return
    }
    setPanel({ name, body: '', loading: true, err: '' })
    try {
      const c = await capabilityApi.get(name)
      setPanel({
        name,
        body: c.body || '（这条能力没有 SKILL.md 正文）',
        loading: false,
        err: '',
      })
    } catch (e) {
      setPanel({ name, body: '', loading: false, err: (e as Error).message })
    }
  }

  const counts = countCapabilities(items)
  const filtered = filterCapabilities(items, { kind, onlyEnabled, query })
  const filteredActive = kind !== 'all' || onlyEnabled || query.trim() !== ''

  const retry = () => void load(full)

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">能力注册表</div>
          <div className="page-sub">
            平台能做什么的唯一清单：API 路由、CLI 命令、skill 三类一屏看全。默认只拉第一层
            （name / kind / description / enabled），打开「显示详情」才请求第二层字段。
            点 Skill 条目展开第三层，读它的 SKILL.md 全文。
          </div>
        </div>
        <button className="btn page-act" onClick={retry}>
          刷新
        </button>
      </div>

      {err && (
        <Notice kind="error">
          读取能力注册表失败（{err}）
          <div className="notice-sub">
            <button className="btn small" onClick={retry}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {disabled ? (
        <DisabledGuide page="capabilities" onRetry={retry} />
      ) : (
        <>
          {items && (
            <div className="loop-counts">
              <Stat k="API" v={counts.api} tone="api" />
              <Stat k="CLI" v={counts.cli} tone="cli" />
              <Stat k="Skill" v={counts.skill} tone="skill" />
              <Stat k="已启用" v={counts.enabled} tone="on" />
              <Stat k="未启用" v={counts.disabled} tone="off" />
            </div>
          )}

          <div className="card">
            <div className="card-head">筛选</div>
            <div className="card-body">
              <div className="loop-filters">
                <div className="seg" role="group" aria-label="按 kind 筛选">
                  {KINDS.map((k) => (
                    <button
                      key={k.value}
                      type="button"
                      className={`seg-item ${kind === k.value ? 'on' : ''}`}
                      onClick={() => setKind(k.value)}
                    >
                      {k.label}
                    </button>
                  ))}
                </div>
                <label className="check">
                  <input
                    type="checkbox"
                    checked={onlyEnabled}
                    onChange={(e) => setOnlyEnabled(e.target.checked)}
                  />
                  只看已启用
                </label>
                <input
                  className="input"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="搜索名称 / 描述 / endpoint / requires"
                  spellCheck={false}
                  aria-label="关键字搜索"
                />
                <label className="check" title="切到 ?detail=full，多出 endpoint / requiredEnv / requires / unavailable 列">
                  <input type="checkbox" checked={full} onChange={toggleFull} />
                  显示详情（?detail=full）
                </label>
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              能力清单
              <span className="dim small">
                {items === null ? ' · 加载中' : ` · 共 ${items.length} 条，当前显示 ${filtered.length} 条`}
              </span>
            </div>

            {items === null ? (
              <Skel rows={5} />
            ) : filtered.length === 0 ? (
              <Empty text={emptyText('capabilities', filteredActive)} />
            ) : (
              <div className="tbl-wrap loop-table">
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>名称</th>
                      <th style={{ width: 70 }}>类型</th>
                      <th style={{ width: 84 }}>状态</th>
                      <th>描述</th>
                      <th style={{ width: 100 }}>来源</th>
                      {full && <th>endpoint</th>}
                      {full && <th>requiredEnv</th>}
                      {full && <th>requires</th>}
                      {full && <th>unavailable</th>}
                    </tr>
                  </thead>
                  <tbody>
                    {filtered.map((c) => (
                      <tr
                        key={c.name}
                        className={c.kind === 'skill' ? 'link' : ''}
                        title={
                          c.kind === 'skill'
                            ? '点击展开该 skill 的 SKILL.md 正文'
                            : undefined
                        }
                        onClick={() => {
                          if (c.kind === 'skill') void openSkill(c.name)
                        }}
                      >
                        <td className="mono small">{c.name}</td>
                        <td>
                          <span className="dot-tag dim">{kindLabel(c.kind)}</span>
                        </td>
                        <td>
                          <span className={`loop-tag ${c.enabled ? 'loop-tag-ready' : 'loop-tag-warn'}`}>
                            {c.enabled ? '启用' : '未启用'}
                          </span>
                        </td>
                        <td>
                          <span className="loop-cell-cut" title={c.description || ''}>
                            {c.description || '—'}
                          </span>
                        </td>
                        <td className="loop-cell-mono">{c.source || '—'}</td>
                        {full && (
                          <td>
                            <span className="loop-cell-mono loop-cell-cut" title={c.endpoint || ''}>
                              {c.endpoint || '—'}
                            </span>
                          </td>
                        )}
                        {full && <td><List values={c.requiredEnv} /></td>}
                        {full && <td><List values={c.requires} /></td>}
                        {full && (
                          <td>
                            <span className="loop-cell-cut" title={c.unavailable || ''}>
                              {c.unavailable || '—'}
                            </span>
                          </td>
                        )}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          {panel && (
            <div className="loop-panel">
              <div className="loop-panel-head">
                <span className="loop-panel-name">SKILL.md · {panel.name}</span>
                <button className="btn small" onClick={() => setPanel(null)}>
                  收起
                </button>
              </div>
              {panel.loading ? (
                <Skel rows={3} />
              ) : panel.err ? (
                <Notice kind="error">{panel.err}</Notice>
              ) : (
                <pre className="loop-body">{panel.body}</pre>
              )}
            </div>
          )}
        </>
      )}
    </div>
  )
}

function Stat({ k, v, tone }: { k: string; v: number; tone: string }) {
  return (
    <div className={`stat loop-c-${tone}`}>
      <div className="stat-k">{k}</div>
      <div className="stat-v">{v}</div>
    </div>
  )
}

function List({ values }: { values?: string[] }) {
  if (!values || values.length === 0) return <span className="dim">—</span>
  return (
    <span className="loop-list">
      {values.map((v) => (
        <span className="dot-tag dim" key={v}>
          {v}
        </span>
      ))}
    </span>
  )
}
