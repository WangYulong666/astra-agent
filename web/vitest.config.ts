import { defineConfig } from 'vitest/config'

// 只测纯逻辑（timeline / router / format），没有 DOM 交互，所以：
// - environment 用 node，不引 jsdom / happy-dom；
// - 不引 @vitejs/plugin-react：被测文件里没有 JSX，React 插件只会拖慢启动；
// - 不设 globals：测试文件显式 import { describe, it, expect }，tsc -b 才能解析到类型。
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
