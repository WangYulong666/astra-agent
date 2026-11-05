/* Astra 的离线壳。
 *
 * 只缓存 app shell（`/` 与 /assets/* 的构建产物），绝不缓存 /v1/：
 * 控制面返回的是带身份的实时数据，缓存它只会把「登录过期了」变成
 * 「界面还在，但显示的全是旧的」——那比白屏更难排查。
 *
 * 换版本只需要改 VERSION：install 会写新缓存，activate 会清掉所有
 * 前缀相同但名字不同的旧缓存。
 */

const VERSION = 'astra-shell-2026-10-30.1'
const CACHE = `astra-${VERSION}`
const CACHE_PREFIX = 'astra-'

// 装 SW 时就该有的东西。图标也放进来：离线时从主屏启动，图标还是要有。
const SHELL = ['/', '/manifest.webmanifest', '/icon-192.png', '/icon-512.png', '/apple-touch-icon.png']

self.addEventListener('install', (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE)
      // 逐个 add 而不是 addAll：addAll 是全有全无，一个 404 会让整个 install 失败，
      // 结果就是「明明只想少缓存一张图标，却连离线能力都没有」。
      await Promise.all(SHELL.map((u) => cache.add(u).catch(() => {})))
      await self.skipWaiting()
    })(),
  )
})

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      const keys = await caches.keys()
      await Promise.all(
        keys
          .filter((k) => k.startsWith(CACHE_PREFIX) && k !== CACHE)
          .map((k) => caches.delete(k)),
      )
      await self.clients.claim()
    })(),
  )
})

self.addEventListener('fetch', (event) => {
  const req = event.request
  // 非 GET（创建会话、发指令、审批）一律直接走网络，不经过这里。
  if (req.method !== 'GET') return

  const url = new URL(req.url)
  // 跨域请求不接管：接管的收益是 0，出错时排查成本是 100。
  if (url.origin !== self.location.origin) return

  // 控制面：不碰。SSE（/v1/runs/:id/events）尤其不能碰——
  // 被 SW 读一遍就会把流式事件攒成一次性响应，界面会「卡住然后一次刷完」。
  if (url.pathname.startsWith('/v1/')) return
  if (url.pathname === '/healthz') return
  if (url.pathname === '/sw.js') return

  // 文件名里带内容哈希，命中就一定是正确的版本，直接给缓存。
  if (url.pathname.startsWith('/assets/')) {
    event.respondWith(cacheFirst(req))
    return
  }

  // 导航请求（打开页面 / 刷新）：优先拿最新的 index.html，
  // 断网时回退到上次缓存的 `/`。
  if (req.mode === 'navigate') {
    event.respondWith(navigateFirst(req))
  }
})

async function cacheFirst(req) {
  const cache = await caches.open(CACHE)
  const hit = await cache.match(req)
  if (hit) return hit
  const res = await fetch(req)
  if (res.ok) await cache.put(req, res.clone())
  return res
}

async function navigateFirst(req) {
  try {
    const res = await fetch(req)
    if (res.ok && res.type === 'basic') {
      // 不 await：更新缓存失败不该拖慢这一次导航。
      void storeShell(res.clone())
    }
    return res
  } catch {
    const cache = await caches.open(CACHE)
    const shell = await cache.match('/')
    if (shell) return shell
    return new Response('离线，而且本地还没有缓存过界面。请联网打开一次。', {
      status: 503,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    })
  }
}

/**
 * 把 index.html 存成 `/`，顺手把它引用的 /assets/* 一起存下来。
 * 只存 HTML 是不够的：那份 HTML 指向的 JS/CSS 如果没缓存，离线时
 * 拿到的就是一个空壳，白屏比报错更让人迷惑。
 */
async function storeShell(res) {
  const cache = await caches.open(CACHE)
  const html = await res.text()
  await cache.put('/', new Response(html, { headers: res.headers }))
  const urls = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((m) => m[1])
  await Promise.all(urls.map((u) => cache.add(u).catch(() => {})))
}
