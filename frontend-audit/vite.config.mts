/**
 * 前端增强构建入口：复用原始 frontend/vite.config.ts，追加归档查看补丁。
 *
 * 用法（在 frontend/ 目录执行）：
 *   pnpm exec vite build --config ../frontend-audit/vite.config.mts
 *
 * 原始 Vue/TS 源文件不做任何修改；不使用该配置时构建产物与上游完全一致。
 */
import { defineConfig, mergeConfig, type ConfigEnv, type UserConfig } from 'vite'
import type { ManualChunksFunction } from 'rollup'
import { resolve } from 'path'
import baseConfig from '../frontend/vite.config'
import { auditUsageTablePatch } from './plugin/usageTablePatch.mjs'
import { auditDepsAlias, auditDepsDir } from './deps/alias.mjs'

export default defineConfig(async (env: ConfigEnv) => {
  const base = (typeof baseConfig === 'function' ? await baseConfig(env) : baseConfig) as UserConfig
  // 原配置把 node_modules 统一并入首页预加载的 vendor 包；补丁自带依赖（Monaco）单独成包，
  // 只在打开归档正文时按需加载，首页体积不变。
  const output = base.build?.rollupOptions?.output
  const baseChunks = !Array.isArray(output) && typeof output?.manualChunks === 'function' ? output.manualChunks : undefined
  const manualChunks: ManualChunksFunction = (id, meta) => {
    if (id.startsWith(auditDepsDir)) return 'audit-monaco'
    // Vite 的按需加载辅助函数 Monaco 也会用到：固定单独成包，避免被并进 Monaco 包后连带进首页
    if (id.includes('vite/preload-helper')) return 'vite-preload'
    return baseChunks?.(id, meta)
  }
  return mergeConfig(base, {
    build: { rollupOptions: { output: { manualChunks } } },
    plugins: [auditUsageTablePatch()],
    resolve: {
      alias: [{ find: '@audit', replacement: resolve(__dirname, 'src') }, ...auditDepsAlias()],
      // 补丁目录不带 node_modules：依赖统一从 frontend/ 解析，避免出现第二份 vue。
      dedupe: ['vue', 'vue-router', 'vue-i18n', 'axios']
    }
  } satisfies UserConfig)
})
