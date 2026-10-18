import { defineConfig } from 'vitest/config'
import { resolve } from 'path'
import vue from '@vitejs/plugin-vue'
import { auditDepsAlias } from './deps/alias.mjs'

// 补丁自身的单元测试（在 frontend/ 依赖之上运行，不改动原有测试配置）。
export default defineConfig({
  root: __dirname,
  plugins: [vue()],
  resolve: {
    alias: [
      { find: '@audit', replacement: resolve(__dirname, 'src') },
      { find: /^@\//, replacement: `${resolve(__dirname, '../frontend/src')}/` },
      { find: 'vue-i18n', replacement: 'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js' },
      ...auditDepsAlias()
    ]
  },
  test: {
    globals: true,
    environment: 'jsdom',
    include: ['__tests__/**/*.spec.ts']
  }
})
