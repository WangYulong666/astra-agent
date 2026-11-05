// 管理后台「设置」页。
//
// 三块内容放一页是有意的：排障时「agent 现在是什么模式、key 还在不在、
// 上游通不通」是同一条思考线，拆成三页就要来回跳。
//
// 一个必须写清的语义（后端 internal/api/setup.go 就这么实现的，前端不替它圆谎）：
// POST /v1/admin/llm-settings：key 留空时走 keepKey=true（保留现有 key 只改 baseUrl），
// 清除走显式 clear=true。空 key 且不带任何标志仍按清除处理（向后兼容）。

import { useCallback, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { api, setupApi } from '../../api'
import type { LLMSettings } from '../../api'
import { useAuth } from '../../auth'
import { Field, Notice } from '../../components/ui'

/** 审批粒度的中文解释。后端只回枚举值，人话在这里补。 */
const APPROVAL_TEXT: Record<string, string> = {
  'per-tool': '逐条审批：每个工具调用都要人工确认后才执行',
  escalation: '扩权审批：常规操作自动执行，越出沙箱/提权的操作才问人',
  none: '不审批：所有工具调用直接执行，不问人',
}

export function Settings() {
  const { user } = useAuth()

  // 运行时信息来自 /v1/meta：agentName/policy/approval 是全局配置，
  // email/role 只有认证模式下才有（兼容模式落到 useAuth 的本地值，通常为空）。
  const [meta, setMeta] = useState<Record<string, string> | null>(null)
  const [metaErr, setMetaErr] = useState('')

  const [settings, setSettings] = useState<LLMSettings | null>(null)
  const [settingsErr, setSettingsErr] = useState('')

  const load = useCallback(async () => {
    try {
      setSettings(await setupApi.llmSettings())
      setSettingsErr('')
    } catch (e) {
      setSettingsErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
    void (async () => {
      try {
        setMeta(await api.meta())
        setMetaErr('')
      } catch (e) {
        setMetaErr((e as Error).message)
      }
    })()
  }, [load])

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="page-title">设置</div>
          <div className="page-sub">
            运行时配置是只读的（改了要重启进程才生效）；模型网关的 key 和上游地址可以热更，保存后立即生效并落盘。
          </div>
        </div>
      </div>

      <RuntimeCard meta={meta} err={metaErr} user={user} />
      <GatewayCard settings={settings} err={settingsErr} onReload={load} />

      <div className="card">
        <div className="card-head">说明</div>
        <div className="card-body settings-notes">
          <p>
            <strong>key 的存放位置：</strong>
            只存在控制面进程内存与本机数据目录的 key 文件里，持久化落盘也在本机完成。
            key 永远不会进入沙箱、不会出现在会话内容或审计详情里；管理接口也只回「有没有」，不回本体。
          </p>
          <p>
            <strong>agent 模式切换：</strong>
            运行时卡片里的 agent 模式、审批粒度、权限预设都来自启动配置，不支持热改。
            桌面端修改 config.json 后重启应用；服务器端改启动参数（如 <code>-policy</code>、
            <code>-agent</code>）后重启进程。
          </p>
        </div>
      </div>
    </div>
  )
}

function RuntimeCard({
  meta,
  err,
  user,
}: {
  meta: Record<string, string> | null
  err: string
  user: { email?: string; displayName?: string; role?: string } | null
}) {
  const approval = meta?.approval ?? ''
  return (
    <div className="card">
      <div className="card-head">运行时</div>
      <div className="card-body">
        {err && <Notice kind="error">{err}</Notice>}
        {!meta && !err && <div className="dim small">读取中…</div>}
        {meta && (
          <div className="settings-kv">
            <Kv k="Agent 模式" v={meta.agentName || '—'} />
            <Kv
              k="审批粒度"
              v={
                <>
                  <span className="mono small">{approval || '—'}</span>
                  {APPROVAL_TEXT[approval] && (
                    <span className="dim small">（{APPROVAL_TEXT[approval]}）</span>
                  )}
                </>
              }
            />
            <Kv
              k="权限预设（policy）"
              v={<span className="mono">{meta.policy || '—'}</span>}
            />
            <Kv
              k="当前登录用户"
              v={
                <>
                  {meta.email || user?.email || '（兼容模式，未登录）'}
                  <span className="dot-tag dim" style={{ marginLeft: 8 }}>
                    {meta.role || user?.role || '—'}
                  </span>
                </>
              }
            />
          </div>
        )}
      </div>
    </div>
  )
}

function Kv({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="settings-kv-row">
      <div className="settings-kv-key">{k}</div>
      <div className="settings-kv-val">{v}</div>
    </div>
  )
}

type TestResult = { busy: boolean; ok?: boolean; latencyMs?: number; error?: string }

function GatewayCard({
  settings,
  err,
  onReload,
}: {
  settings: LLMSettings | null
  err: string
  onReload: () => Promise<void>
}) {
  const [baseUrl, setBaseUrl] = useState('')
  const [newKey, setNewKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [saveErr, setSaveErr] = useState('')
  const [savedMsg, setSavedMsg] = useState('')
  const [test, setTest] = useState<TestResult>({ busy: false })

  // baseUrl 只在 settings 变化时回填：保存/清除后的重载会让输入框跟上服务端的真值。
  useEffect(() => {
    if (settings) setBaseUrl(settings.baseUrl || '')
  }, [settings])

  if (!settings) {
    return (
      <div className="card">
        <div className="card-head">模型网关</div>
        <div className="card-body">
          {err ? <Notice kind="error">{err}</Notice> : <div className="dim small">读取中…</div>}
        </div>
      </div>
    )
  }

  const save = async () => {
    setSaveErr('')
    setSavedMsg('')
    const key = newKey.trim()
    // 留空 + 已有 key = keepKey：保留 key、只更新 baseUrl。不再需要拿旧 key 重发。
    const keep = !key && settings.hasKey
    setBusy(true)
    try {
      const res = await setupApi.saveLLMSettings({
        apiKey: key,
        keepKey: keep || undefined,
        ...(baseUrl.trim() ? { baseUrl: baseUrl.trim() } : {}),
      })
      setNewKey('')
      setSavedMsg(keep ? '已保存并生效（key 保持不变）' : res.hasKey ? '已保存并生效（key 已配置）' : '已保存并生效（当前无 key）')
      await onReload()
    } catch (e) {
      setSaveErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const clearKey = async () => {
    setSaveErr('')
    setSavedMsg('')
    const ok = window.confirm(
      '确定清除已配置的 key？\n\n' +
        '清除后网关仍在运行，但所有上游请求会因无凭据而失败（502），直到重新配好 key。',
    )
    if (!ok) return
    setBusy(true)
    try {
      await setupApi.saveLLMSettings({ apiKey: '' })
      setNewKey('')
      setSavedMsg('key 已清除')
      await onReload()
    } catch (e) {
      setSaveErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const runTest = async () => {
    setSaveErr('')
    setSavedMsg('')
    setTest({ busy: true })
    try {
      // key 留空时后端会测「已存的那把」——正好覆盖「系统里存的还好使吗」。
      const res = await setupApi.testLLM({
        apiKey: newKey.trim(),
        ...(baseUrl.trim() ? { baseUrl: baseUrl.trim() } : {}),
      })
      setTest({ busy: false, ok: res.ok, latencyMs: res.latencyMs, error: res.error })
    } catch (e) {
      setTest({ busy: false, ok: false, error: (e as Error).message })
    }
  }

  return (
    <div className="card">
      <div className="card-head">模型网关</div>
      <div className="card-body">
        <div className="settings-gateway-state">
          <span className={`dot-tag ${settings.gateway ? 'ok' : 'off'}`}>
            {settings.gateway ? '网关已启用' : '网关未启用'}
          </span>
          <span className={`dot-tag ${settings.hasKey ? 'ok' : 'off'}`}>
            {settings.hasKey ? '已配置 key' : '未配置 key'}
          </span>
          {savedMsg && <span className="settings-flash">{savedMsg}</span>}
        </div>

        {!settings.gateway && (
          <Notice kind="warn">
            网关未启用（启动时未开 llm-gateway），这里的修改会被服务端拒绝。
          </Notice>
        )}
        {err && <Notice kind="error">{err}</Notice>}
        {saveErr && <Notice kind="error">{saveErr}</Notice>}

        <div className="settings-form">
          <Field label="上游 baseUrl" hint="留空保存=沿用默认（DeepSeek 官方地址）">
            <input
              className="input"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
              placeholder="https://api.deepseek.com"
              spellCheck={false}
            />
          </Field>
          <Field
            label="新的 key"
            hint={
              settings.hasKey
                ? '已存有 key。留空点「保存」= 清除它（确认框会再提醒）；测试时留空=测已存的那把'
                : '服务端只保存、不回显；测试时留空将报「缺少 apiKey」'
            }
          >
            <input
              className="input"
              type="password"
              value={newKey}
              onChange={(e) => setNewKey(e.target.value)}
              placeholder={settings.hasKey ? '········（不回显）' : 'sk-…'}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        </div>

        {test.busy && (
          <Notice kind="info">正在连上游…（最小补全请求，最多等 20 秒）</Notice>
        )}
        {!test.busy && test.ok === true && (
          <Notice kind="info">测试通过：上游可达，耗时 {test.latencyMs} ms</Notice>
        )}
        {!test.busy && test.ok === false && (
          <Notice kind="error">测试失败：{test.error || '未知错误'}</Notice>
        )}

        <div className="settings-actions">
          <button className="btn" disabled={busy || test.busy} onClick={() => void runTest()}>
            {test.busy ? '测试中…' : '测试'}
          </button>
          <button className="btn-primary" disabled={busy || test.busy} onClick={() => void save()}>
            {busy ? '保存中…' : '保存'}
          </button>
          {settings.hasKey && (
            <button
              className="btn small danger"
              disabled={busy || test.busy}
              onClick={() => void clearKey()}
            >
              清除 key
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
