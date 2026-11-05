import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 产物直接写进 Go 包的 embed 目录，`make web` 之后 `go build` 就能带上。
// 开发时用 vite 的代理连 7788，避免每次改前端都要重启 Go 进程。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/api/web/dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': { target: 'http://127.0.0.1:7788', changeOrigin: true },
      '/healthz': { target: 'http://127.0.0.1:7788', changeOrigin: true },
      // SSE 必须关掉缓冲，否则代理层会把事件攒起来一次性吐给浏览器。
      '/v1/runs': { target: 'http://127.0.0.1:7788', changeOrigin: true },
    },
  },
})
