import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import { createMemoryHistory, createRouter } from 'vue-router'
import { defineComponent, h, nextTick } from 'vue'

const fetchUsageArchive = vi.fn()
vi.mock('../src/api', async (orig) => ({ ...(await orig<typeof import('../src/api')>()), fetchUsageArchive: (...a: unknown[]) => fetchUsageArchive(...a) }))
vi.mock('@/api/client', () => ({ apiClient: { get: vi.fn() } }))
vi.mock('../src/monaco', () => import('./fakeMonaco'))

import AuditArchiveEntry from '../src/AuditArchiveEntry.vue'

function archive(usageId: number, ref: string) {
  return {
    usage: { id: usageId, request_id: `client:req-${usageId}`, api_key_id: 1, account_id: 2, user_id: 3, created_at: '' },
    candidates: [{ ref, entries: [], score: 3, match_reason: ['client_request_id'] }],
    resolution: 'unique',
    ref,
    archive: {
      ref,
      status: 'complete',
      summary: { request_id: `req-${usageId}`, session_id: 'sess', trace: { trace_id: 't', span_id: 's', source: 'generated' } },
      attempts: [{ seq: 1, account_id: 2, status: 200, complete: true, request_sections: [0], response_sections: [0] }],
      files: ['client_request', 'upstream_request', 'upstream_response', 'client_response'].map((kind) => ({
        kind, exists: true, compressed_size: 10, raw_size: 20, status: 'ok',
        sections: [{ index: 0, seq: kind.startsWith('upstream') ? 1 : 0, header: { kind, seq: 0, time: '', method: 'POST', url: `/u/${usageId}` }, complete: true, body_bytes: 2, body_sha256: 'ab', hash_match: true, end: { complete: true, bytes: 2, xxh64: 'ab', time: '' }, body: { decoded: false, is_text: true, text: `{"usage":${usageId}}`, truncated_for_display: false } }]
      }))
    }
  }
}

function setup(rowId: number) {
  const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: {} } })
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { render: () => null } }] })
  const Host = defineComponent({
    props: { row: { type: Object, required: true } },
    setup: (p) => () => h(AuditArchiveEntry, { row: p.row })
  })
  const wrapper = mount(Host, { props: { row: { id: rowId } }, global: { plugins: [i18n, router] }, attachTo: document.body })
  return { wrapper, router }
}

describe('archive viewer', () => {
  beforeEach(() => {
    fetchUsageArchive.mockReset()
    document.body.innerHTML = ''
  })

  it('loads the archive for exactly the clicked row', async () => {
    fetchUsageArchive.mockImplementation(async (id: number) => archive(id, `2026-10/14/sess/req-${id}-1`))
    const { wrapper } = setup(7)
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    expect(fetchUsageArchive).toHaveBeenCalledWith(7, undefined, expect.anything())
    expect(document.body.querySelector('[data-testid="audit-archive-status"]')?.textContent).toContain('完整')
    // 正文原文交给编辑器（Monaco 以替身运行），由编辑器内置格式化排版
    await vi.waitFor(() => expect(document.body.textContent).toContain('"usage": 7'))
  })

  it('closes when the row under the entry changes (pagination / sort reuse)', async () => {
    fetchUsageArchive.mockImplementation(async (id: number) => archive(id, `2026-10/14/sess/req-${id}-1`))
    const { wrapper } = setup(7)
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({ row: { id: 8 } })
    await nextTick()
    expect(document.body.querySelector('[data-testid="audit-archive-modal"]')).toBeNull()
  })

  it('ignores a response that belongs to another usage record', async () => {
    fetchUsageArchive.mockImplementation(async () => archive(99, '2026-10/14/sess/req-99-1'))
    const { wrapper } = setup(7)
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    expect(document.body.textContent).not.toContain('req-99')
  })

  it('closes on route navigation', async () => {
    fetchUsageArchive.mockImplementation(async (id: number) => archive(id, `2026-10/14/sess/req-${id}-1`))
    const { wrapper, router } = setup(7)
    await router.push('/admin/usage')
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    await router.push('/admin/usage?page=2')
    await flushPromises()
    expect(document.body.querySelector('[data-testid="audit-archive-modal"]')).toBeNull()
  })

  it.each([
    [{ status: 403, message: 'forbidden' }, '无权限'],
    [{ status: 503, reason: 'AUDIT_ARCHIVE_DISABLED' }, '未启用'],
    [{ status: 404, reason: 'USAGE_NOT_FOUND' }, '用量记录不存在']
  ])('shows explicit error state %#', async (err, text) => {
    fetchUsageArchive.mockRejectedValue(err)
    const { wrapper } = setup(7)
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    expect(document.body.querySelector('[data-testid="audit-archive-error"]')?.textContent).toContain(text)
  })

  it('shows not-found and ambiguous resolutions', async () => {
    fetchUsageArchive.mockResolvedValueOnce({ usage: { id: 7, request_id: 'client:x' }, candidates: [], resolution: 'not_found' })
    const { wrapper } = setup(7)
    await wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    expect(document.body.querySelector('[data-testid="audit-archive-not-found"]')).not.toBeNull()

    document.body.innerHTML = ''
    fetchUsageArchive.mockResolvedValueOnce({
      usage: { id: 8, request_id: 'client:dup' },
      candidates: [
        { ref: '2026-10/14/nosession/dup-1', entries: [], score: 1, match_reason: ['client_request_id'] },
        { ref: '2026-10/14/nosession/dup-2', entries: [], score: 1, match_reason: ['client_request_id'] }
      ],
      resolution: 'ambiguous'
    })
    const second = setup(8)
    await second.wrapper.get('[data-testid="audit-archive-open"]').trigger('click')
    await flushPromises()
    const cands = document.body.querySelector('[data-testid="audit-archive-candidates"]')
    expect(cands?.querySelectorAll('li').length).toBe(2)
  })
})
