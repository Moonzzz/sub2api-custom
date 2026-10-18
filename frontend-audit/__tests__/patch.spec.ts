import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { patchUsageTable } from '../plugin/usageTablePatch.mjs'

const source = readFileSync(resolve(__dirname, '../../frontend/src/components/admin/usage/UsageTable.vue'), 'utf8')

describe('usage table build patch', () => {
  it('mounts the archive entry in the always-visible created_at cell, bound to the row', () => {
    const out = patchUsageTable(source)
    expect(out).toContain('<template #cell-created_at="{ value, row }">')
    expect(out).toContain('<AuditArchiveEntry :row="row" />')
    expect(out).toContain("import AuditArchiveEntry from '@audit/AuditArchiveEntry.vue'")
  })

  it('fails loudly when the baseline anchor disappears', () => {
    expect(() => patchUsageTable(source.replace('#cell-created_at', '#cell-time'))).toThrow(/anchor/)
  })
})
