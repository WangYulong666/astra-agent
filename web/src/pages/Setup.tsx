// 首启向导：创建管理员 → 配模型 key（可跳过）→ 进工作台。
//
// 只在 /v1/setup/state 报 needsSetup 时出现（Root.tsx 负责判断）。
// 两步刻意拆开而不是一张大表单：key 要真连上游验证，等 1~2 秒很正常，
// 混在一起的话「账号建好了但 key 测试失败」的状态没法表达。

import { useState } from 'react'
import { setupApi, ApiError } from '../api'
import { Field, Notice } from '../components/ui'

type Step = 'account' | 'llm' | 'done'

function describe(e: unknown): string {
  if (!(e instanceof ApiError)) return (e as Error)?.message || '操作失败'
  if (e.status === 0) return '网络不通：连不上控制面。'
  if (e.status === 409) return '系统已初始化过，请刷新后直接登录。'
  return e.message
}

export function Setup() {
  const [step, setStep] = useState<Step>('account')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  // 第一步：管理员
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  // 第二步：模型 key（可跳过）
  const [apiKey, setApiKey] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [testResult, setTestResult] = useState('')

  async function createAccount() {
    setBusy(true)
    setErr('')
    try {
      const res = await setupApi.complete({ email, password })
      if (!res.autoLogin || !res.token) throw new Error('自动登录失败，请刷新后用新账号登录')
      // 向导自己管理 token：写进 localStorage 与登录行为一致，
      // 但不动 auth.ts 的状态机——第三步保存 key 后整页刷新最干净。
      localStorage.setItem('astra_token', res.token)
      localStorage.setItem('astra_user', JSON.stringify(res.user))
      localStorage.setItem('astra_tenant', JSON.stringify(res.tenant))
      setStep('llm')
    } catch (e) {
      setErr(describe(e))
    } finally {
      setBusy(false)
    }
  }

  async function testKey() {
    setBusy(true)
    setErr('')
    setTestResult('')
    try {
      const r = await setupApi.testLLM({ apiKey, baseUrl: baseUrl || undefined })
      setTestResult(
        r.ok
          ? `✓ 连接成功（${r.latencyMs ?? '?'}ms）`
          : `✗ ${r.error || '上游拒绝'}`,
      )
    } catch (e) {
      setTestResult(`✗ ${describe(e)}`)
    } finally {
      setBusy(false)
    }
  }

  async function saveKey() {
    setBusy(true)
    setErr('')
    try {
      if (apiKey) await setupApi.saveLLMSettings({ apiKey, baseUrl: baseUrl || undefined })
      setStep('done')
      // 整页刷新让 auth 状态机重新探测，token 已在 localStorage 里。
      window.location.hash = '#/'
      window.location.reload()
    } catch (e) {
      setErr(describe(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="setup-wrap">
      <form
        className="setup-card"
        onSubmit={(e) => {
          e.preventDefault()
          if (step === 'account') void createAccount()
          else if (step === 'llm') void saveKey()
        }}
      >
        <h1 className="setup-title">初始化 Astra</h1>
        <ol className="setup-steps">
          <li className={step === 'account' ? 'on' : 'done'}>
            管理员账号
          </li>
          <li className={step === 'llm' ? 'on' : step === 'done' ? 'done' : ''}>模型 key</li>
          <li className={step === 'done' ? 'on' : ''}>完成</li>
        </ol>

        {err && <Notice kind="error">{err}</Notice>}

        {step === 'account' && (
          <>
            <p className="setup-hint">这是这台 Astra 的第一个管理员账号。</p>
            <Field label="邮箱">
              <input
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="you@example.com"
                autoFocus
              />
            </Field>
            <Field label="密码（至少 8 位）">
              <input
                type="password"
                required
                minLength={8}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="为管理员设置密码"
              />
            </Field>
            <button className="btn primary" disabled={busy}>
              {busy ? '创建中…' : '创建并继续'}
            </button>
          </>
        )}

        {step === 'llm' && (
          <>
            <p className="setup-hint">
              粘贴 DeepSeek API key（<code>sk-…</code>）。key 只保存在本机控制面，
              不会进沙箱。没有 key 也可以先跳过——agent 会以 shell 模式运行。
            </p>
            <Field label="API Key">
              <input
                type="password"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder="sk-…"
              />
            </Field>
            <Field label="上游地址（可选，默认官方）">
              <input
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder="https://api.deepseek.com"
              />
            </Field>
            <div className="setup-row">
              <button
                type="button"
                className="btn"
                onClick={() => void testKey()}
                disabled={busy || !apiKey}
              >
                测试连接
              </button>
              <button type="submit" className="btn primary" disabled={busy}>
                保存并完成
              </button>
              <button
                type="button"
                className="btn ghost"
                onClick={() => setStep('done')}
                disabled={busy}
              >
                跳过
              </button>
            </div>
            {testResult && (
              <Notice kind={testResult.startsWith('✓') ? 'info' : 'error'}>{testResult}</Notice>
            )}
          </>
        )}

        {step === 'done' && (
          <>
            <p className="setup-hint">初始化完成，正在进入工作台…</p>
            <button
              type="button"
              className="btn primary"
              onClick={() => {
                window.location.hash = '#/'
                window.location.reload()
              }}
            >
              进入工作台
            </button>
          </>
        )}
      </form>
    </div>
  )
}
