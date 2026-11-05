// 能力包（Skills）。
//
// 这一页的数据来源是仓库里的 skills/<name>/SKILL.md，**控制面没有对应接口**：
// GET /v1/meta 只暴露 provider/agent/requireAuth 这些运行信息，没有 skills 目录。
// 所以清单在前端内置一份（三个首批能力包），并留一条「meta 里若将来带了
// skills 字段就优先用它」的解析——那时只需要后端加一个键，这个页面不用改。
//
// 页面的第一句话很重要：skills 是给 **agent** 用的能力包，不是给人点的按钮。
// 不写清这点，运营会以为「装了就多一个菜单」，然后找不到它。

import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../../api'
import { copyText, Notice } from '../../components/ui'

type Skill = {
  name: string
  description: string
  requires: string[]
}

/** 首批三个能力包（见 docs/design/platform-extension.md 第六节）。 */
const BUILTIN: Skill[] = [
  {
    name: 'astra-memory',
    description: '检索用户历史记忆与事实，用于回答「上次那个方案」类问题',
    requires: ['astra-cli'],
  },
  {
    name: 'astra-run',
    description: '提交任务到控制面并等待终态，用于把一次性工作交给 agent 执行',
    requires: ['astra-cli'],
  },
  {
    name: 'astra-files',
    description: '读写会话工作区里的文件，用于产出交付物与取回结果',
    requires: ['astra-cli'],
  },
]

export function Skills() {
  const [skills, setSkills] = useState<Skill[] | null>(null)
  const [err, setErr] = useState('')
  const [toast, setToast] = useState('')
  const timer = useRef<number | undefined>(undefined)

  const say = (msg: string) => {
    setToast(msg)
    if (timer.current) window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setToast(''), 2000)
  }

  useEffect(() => {
    return () => {
      if (timer.current) window.clearTimeout(timer.current)
    }
  }, [])

  const load = useCallback(async () => {
    setErr('')
    setSkills(null)
    try {
      const meta = await api.meta()
      // 后端将来若把目录塞进 meta（逗号分隔的名字），以它为准；
      // 描述仍用内置表，缺失的名字退化成「仓库能力包」。
      const listed = (meta.skills || '')
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
      setSkills(listed.length ? listed.map(lookup) : BUILTIN)
    } catch (e) {
      // 读不到 meta 不影响看清单——这页本来就没有依赖它的数据。
      setErr((e as Error).message)
      setSkills(BUILTIN)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const copy = async (name: string) => {
    const cmd = `astra-cli skills install ${name}`
    const ok = await copyText(cmd)
    say(ok ? '已复制安装命令' : '浏览器不允许自动复制，请手动选中命令复制')
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">能力包</div>
          <div className="page-sub">
            skills 是给 agent 用的能力包，不是给人点的按钮：每个包是一份 SKILL.md，说明「什么时候用、
            怎么调用平台」。装上之后 agent 在合适的时候自己装载，通过 astra-cli 调平台能力。
          </div>
        </div>
        <button className="btn page-act" onClick={() => void load()}>
          刷新
        </button>
      </div>

      {err && (
        <Notice kind="warn">
          读不到 <span className="mono">/v1/meta</span>（{err}），下面是前端内置的清单，内容不受影响。
          <div className="notice-sub">
            <button className="btn small" onClick={() => void load()}>
              重试
            </button>
          </div>
        </Notice>
      )}

      <div className="card">
        <div className="card-head">仓库内置能力包</div>

        {skills === null ? (
          <Skel rows={3} />
        ) : skills.length === 0 ? (
          <div className="tbl-empty">这个仓库还没有能力包（skills/ 目录为空）</div>
        ) : (
          <div>
            {skills.map((s) => (
              <div className="skill-item" key={s.name}>
                <div className="skill-head">
                  <span className="skill-name">{s.name}</span>
                  {s.requires.map((r) => (
                    <span className="dot-tag dim" key={r}>
                      需要 {r}
                    </span>
                  ))}
                </div>
                <div className="skill-desc">{s.description}</div>
                <div className="cmd-line">
                  <code>astra-cli skills install {s.name}</code>
                  <button className="btn small" onClick={() => void copy(s.name)}>
                    复制安装命令
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">说明</div>
        <div className="card-body settings-notes">
          <p>
            清单来自仓库 <code>skills/&lt;name&gt;/SKILL.md</code> 的 frontmatter（name / description /
            requires）。控制面目前没有列出 skills 的接口，这一页的清单因此是内置的；
            新增能力包时要同步改这里，或者等 <code>/v1/meta</code> 加上 <code>skills</code> 字段（本页已优先读它）。
          </p>
          <p>
            安装命令由 <code>astra-cli</code> 执行：它把 SKILL.md 放进 agent 的能力目录，不安装任何可执行文件，
            也不改平台配置。卸载 = 删掉那个目录。
          </p>
        </div>
      </div>

      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}

/** 名字在 meta 里出现但内置表没有：给一个如实的占位，不编描述。 */
function lookup(name: string): Skill {
  return (
    BUILTIN.find((s) => s.name === name) || {
      name,
      description: '（前端没有这个能力包的描述，见仓库 skills 目录）',
      requires: ['astra-cli'],
    }
  )
}

function Skel({ rows }: { rows: number }) {
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
