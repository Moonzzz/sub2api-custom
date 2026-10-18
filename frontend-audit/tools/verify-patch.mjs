// 校验补丁锚点仍能在当前基线的 UsageTable.vue 上应用（不写回任何文件）。
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { patchUsageTable } from '../plugin/usageTablePatch.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const target = resolve(here, '../../frontend/src/components/admin/usage/UsageTable.vue')
const source = readFileSync(target, 'utf8')
const patched = patchUsageTable(source)

const checks = [
  [patched.includes('<template #cell-created_at="{ value, row }">'), 'slot exposes row'],
  [patched.includes('<AuditArchiveEntry :row="row" />'), 'entry mounted in created_at cell'],
  [patched.includes("import AuditArchiveEntry from '@audit/AuditArchiveEntry.vue'"), 'component imported'],
  [patched.split('AuditArchiveEntry :row').length === 2, 'entry mounted exactly once'],
  [readFileSync(target, 'utf8') === source, 'source file untouched'],
]
let failed = false
for (const [ok, name] of checks) {
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}`)
  failed ||= !ok
}
if (failed) process.exit(1)
