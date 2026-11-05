import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { navigate } from './router'

// router.ts 里真正做解析的是私有函数 readHash()，它没有被导出。
// 但 navigate(path) 的第一行就是「readHash() === path 则不写 hash」，
// 所以「navigate 是否改写了 hash」等价于「这段 hash 解析出来是不是 path」——
// 这里就用这个可观测的副作用来断言解析结果，不去动被测源码。
type FakeWindow = { location: { hash: string } }

let win: FakeWindow

function withHash(hash: string) {
  win = { location: { hash } }
  vi.stubGlobal('window', win)
}

beforeEach(() => {
  withHash('')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('hash 路由解析', () => {
  it('空 hash 归一到 #/（navigate("/") 不再改写地址栏）', () => {
    withHash('')
    navigate('/')
    expect(win.location.hash).toBe('')
  })

  it('#/ 就是工作台', () => {
    withHash('#/')
    navigate('/')
    expect(win.location.hash).toBe('#/')
  })

  it('#/admin/users 与 /admin/users 相互匹配', () => {
    withHash('#/admin/users')
    navigate('/admin/users')
    expect(win.location.hash).toBe('#/admin/users')
  })

  it('hash 里带查询串时只认路径部分（#/admin/usage?from=1 → /admin/usage）', () => {
    // 防的回归：把 ?from=1 也算进路径 → 匹配不到 ROUTES → 被兜底打回工作台，
    // 表现是「从用量页刷新一下回到了首页」。
    withHash('#/admin/usage?from=1')
    navigate('/admin/usage')
    expect(win.location.hash).toBe('#/admin/usage?from=1')
  })

  it('未知路径兜底到工作台，不会白屏', () => {
    withHash('#/admin/usrers') // 手输错一个字母
    navigate('/')
    expect(win.location.hash).toBe('#/admin/usrers')

    // 反向确认：此时确实解析成了 '/'，而不是原样保留错路径——
    // 所以 navigate 到错路径本身是不相等的，会真的写一次 hash。
    navigate('/admin/usrers')
    expect(win.location.hash).toBe('/admin/usrers')
  })

  it('缺少前导斜杠的 hash（#admin/users）也算未知路径', () => {
    withHash('#admin/users')
    navigate('/')
    expect(win.location.hash).toBe('#admin/users')
  })

  it('切到另一个合法路由时写入新 hash', () => {
    withHash('#/')
    navigate('/admin/tenants')
    expect(win.location.hash).toBe('/admin/tenants')
  })
})

// useRoute 是 hook + window 事件监听，node 环境（无 DOM/React 渲染器）跑不了；
// 按 D10 不引 jsdom，这里只记录待办。
describe('useRoute', () => {
  it.todo('首屏无 hash 时 replace 成 #/ 并在 hashchange 后重渲染（需要 DOM 环境）')
})
