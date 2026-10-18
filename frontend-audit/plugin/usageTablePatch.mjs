/**
 * 构建期补丁：在用量明细表（UsageTable.vue）的“时间”单元格中挂载归档查看入口。
 *
 * - 只在 Vite 构建管线中对源码做内存变换，磁盘上的原始 Vue/TS 文件保持不变；
 * - “时间”列为 ALWAYS_VISIBLE 列，桌面表格与移动端卡片共用同一个具名插槽，
 *   入口直接绑定行对象（row），分页 / 排序 / 跳转后不会串行；
 * - 锚点缺失（基线变化）时直接让构建失败，而不是静默产出没有入口的产物。
 */
/** @typedef {import('vite').Plugin} Plugin */

export const USAGE_TABLE_SUFFIX = '/src/components/admin/usage/UsageTable.vue'

const SLOT_RE = /<template #cell-created_at="\{ value \}">([\s\S]*?)<\/template>/
const SCRIPT_RE = /<script setup lang="ts">\n/

/**
 * @param {string} code
 * @returns {string}
 */
export function patchUsageTable(code) {
  if (!SLOT_RE.test(code)) {
    throw new Error('[audit-ui] UsageTable.vue: anchor `#cell-created_at="{ value }"` not found; rebase frontend-audit patch onto the new baseline')
  }
  if (!SCRIPT_RE.test(code)) {
    throw new Error('[audit-ui] UsageTable.vue: `<script setup lang="ts">` not found')
  }
  return code
    .replace(
      SLOT_RE,
      (_m, inner) =>
        `<template #cell-created_at="{ value, row }">${inner.replace(/\s+$/, '')}\n          <AuditArchiveEntry :row="row" />\n        </template>`
    )
    .replace(SCRIPT_RE, (m) => `${m}import AuditArchiveEntry from '@audit/AuditArchiveEntry.vue'\n`)
}

/** @returns {Plugin} */
export function auditUsageTablePatch() {
  let patched = false
  return {
    name: 'sub2api-audit-usage-table-patch',
    enforce: 'pre',
    transform(code, id) {
      const file = id.split('?')[0].replace(/\\/g, '/')
      if (!file.endsWith(USAGE_TABLE_SUFFIX) || id.includes('?vue&')) {
        return null
      }
      patched = true
      return { code: patchUsageTable(code), map: null }
    },
    transformIndexHtml(html) {
      return html.replace('</head>', '  <meta name="sub2api-audit-ui" content="1" />\n</head>')
    },
    buildEnd(error) {
      if (!error && !patched && this.meta.watchMode === false) {
        this.error('[audit-ui] UsageTable.vue was never transformed; patch not applied')
      }
    }
  }
}
