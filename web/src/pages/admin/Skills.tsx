// 能力包（Skills）。
//
// 清单来自 GET /v1/skills 的真实注册表（含 distill accept 后新增的动态 skill），
// 不再前端内置。三个容易被做坏的判断写在最前面：
//
// 1. skills 是给 **agent** 用的能力包，不是给人点的按钮——装上不会多一个菜单；
// 2. status=unavailable 要如实展示原因与 requires，不静默假装可用；
// 3. 能力未启用（503）时给「未启用」引导，不画一张看起来「在跑、只是没数据」
//    的空表——那会把运维引去排查 agent，而不是去开能力开关。
//
// 点某项展开时再调 GET /v1/skills/{name} 拿 SKILL.md 全文（第三层披露），
// 纯文本展示，不引 markdown 库。

import { useCallback, useEffect, useRef, useState } from 'react'
import { skillApi } from '../../api'
import type { Skill } from '../../api'
import { copyText, Empty, Notice } from '../../components/ui'
import { fmtTime } from '../../format'
import { emptyText, isUnavailable, skillStatus } from './loopModel'
import { DisabledGuide, Skel } from './LoopGuide'

type DetailState = { body: string; loading: boolean; err: string }

export function Skills() {
  const [items, setItems] = useState<Skill[] | null>(null)
  const [err, setErr] = useState('')
  const [disabled, setDisabled] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [detail, setDetail] = useState<Record<string, DetailState>>({})
  const [toast, setToast] = useState('')
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => {
    return () => {
      if (timer.current) window.clearTimeout(timer.current)
    }
  }, [])

  const say = (msg: string) => {
    setToast(msg)
    if (timer.current) window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setToast(''), 2000)
  }

  const load = useCallback(async () => {
    setErr('')
    setDisabled(false)
    setItems(null)
    setOpen(null)
    setDetail({})
    try {
      const res = await skillApi.list()
      setItems(res.items || [])
    } catch (e) {
      if (isUnavailable(e)) setDisabled(true)
      else setErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const toggle = async (name: string) => {
    if (open === name) {
      setOpen(null)
      return
    }
    setOpen(name)
    if (detail[name]) return
    setDetail((d) => ({ ...d, [name]: { body: '', loading: true, err: '' } }))
    try {
      const s = await skillApi.get(name)
      setDetail((d) => ({
        ...d,
        [name]: { body: s.body || '（这份 SKILL.md 没有正文）', loading: false, err: '' },
      }))
    } catch (e) {
      setDetail((d) => ({
        ...d,
        [name]: { body: '', loading: false, err: (e as Error).message },
      }))
    }
  }

  const copy = async (body: string) => {
    const ok = await copyText(body)
    say(ok ? '已复制正文' : '浏览器不允许自动复制，请手动选中正文复制')
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">能力包</div>
          <div className="page-sub">
            skills 是给 agent 用的能力包，不是给人点的按钮：每个包是一份 SKILL.md，说明「什么时候用、
            怎么调用平台」。清单来自技能注册表，装上之后 agent 在合适的时候自己装载、通过 astra-cli
            调平台能力；distill accept 后新增的动态 skill 会直接出现在这里。
          </div>
        </div>
        <button className="btn page-act" onClick={() => void load()}>
          刷新
        </button>
      </div>

      {err && (
        <Notice kind="error">
          读取技能清单失败（{err}）
          <div className="notice-sub">
            <button className="btn small" onClick={() => void load()}>
              重试
            </button>
          </div>
        </Notice>
      )}

      {disabled ? (
        <DisabledGuide page="skills" onRetry={() => void load()} />
      ) : (
        <>
          <div className="card">
            <div className="card-head">
              技能注册表清单
              <span className="dim small"> · 来自 GET /v1/skills</span>
            </div>

            {items === null ? (
              <Skel rows={4} />
            ) : items.length === 0 ? (
              <Empty text={emptyText('skills', false)} />
            ) : (
              <div>
                {items.map((s) => (
                  <SkillRow
                    key={s.name}
                    skill={s}
                    open={open === s.name}
                    detail={open === s.name ? detail[s.name] : undefined}
                    onToggle={() => void toggle(s.name)}
                    onCopy={() => void copy(detail[s.name]?.body || '')}
                  />
                ))}
              </div>
            )}
          </div>

          <div className="card">
            <div className="card-head">说明</div>
            <div className="card-body settings-notes">
              <p>
                清单来自技能注册表 <code>GET /v1/skills</code>，索引在 <code>SKILL_DIR/index.json</code>，
                accept / reject / 新建都会刷新它。状态 <code>ready</code> 表示依赖已满足、可下发给 agent；
                <code> unavailable</code> 会带出原因与 <code>requires</code>，不满足的 skill 不下发、也不假装可用。
              </p>
              <p>
                点条目展开会再读 <code>GET /v1/skills/{'{name}'}</code> 拿 SKILL.md 全文（渐进式披露的第三层）。
                安装 / 卸载由 <code>astra-cli</code> 或 distill 完成，控制面只提供清单与正文，不在这里改文件。
              </p>
            </div>
          </div>
        </>
      )}

      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}

function SkillRow({
  skill,
  open,
  detail,
  onToggle,
  onCopy,
}: {
  skill: Skill
  open: boolean
  detail?: DetailState
  onToggle: () => void
  onCopy: () => void
}) {
  const st = skillStatus(skill)
  const requires = skill.requires || []
  return (
    <div className="skill-item loop-skill">
      <div
        className="skill-head loop-skill-head"
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={onToggle}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            onToggle()
          }
        }}
      >
        <span className="loop-caret">{open ? '▾' : '▸'}</span>
        <span className="skill-name">{skill.name}</span>
        <span className={`loop-tag loop-tag-${st.tone}`}>{st.label}</span>
        {skill.version && <span className="dot-tag dim">v{skill.version}</span>}
        {skill.source && <span className="dim small">来源 {skill.source}</span>}
        {skill.updatedAt && <span className="dim small">{fmtTime(skill.updatedAt)}</span>}
      </div>

      <div className="skill-desc">{skill.description || '（这份能力包没有描述）'}</div>

      {skill.status !== 'ready' && (
        <div className="loop-reason">
          <span className="loop-tag loop-tag-warn">unavailable</span>
          <span>{skill.unavailable || '依赖未满足，暂不下发给 agent'}</span>
        </div>
      )}

      {requires.length > 0 && (
        <div className="loop-req">
          {requires.map((r) => (
            <span className="dot-tag dim" key={r}>
              需要 {r}
            </span>
          ))}
        </div>
      )}

      {open && (
        <div className="loop-panel">
          <div className="loop-panel-head">
            <span className="loop-panel-name">SKILL.md · {skill.name}</span>
            {detail && !detail.loading && !detail.err && (
              <button className="btn small" onClick={onCopy}>
                复制正文
              </button>
            )}
          </div>
          {!detail || detail.loading ? (
            <Skel rows={3} />
          ) : detail.err ? (
            <Notice kind="error">{detail.err}</Notice>
          ) : (
            <pre className="loop-body">{detail.body}</pre>
          )}
        </div>
      )}
    </div>
  )
}
