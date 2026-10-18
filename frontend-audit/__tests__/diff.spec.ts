import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'

vi.mock('../src/monaco', () => import('./fakeMonaco'))
const fetchFullBodyText = vi.fn()
vi.mock('../src/api', async (orig) => ({ ...(await orig<typeof import('../src/api')>()), fetchFullBodyText: (...a: unknown[]) => fetchFullBodyText(...a) }))
import { calls } from './fakeMonaco'
import AuditSectionPanel from '../src/AuditSectionPanel.vue'
import type { SectionView } from '../src/api'

const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: {} } })

function section(kind: string, seq: number, header: Record<string, string[]>, body: Record<string, unknown>, extra: Record<string, unknown> = {}): SectionView {
  return {
    index: 0, seq, complete: true, body_bytes: 10, body_sha256: 'ab'.repeat(32), hash_match: true,
    header: { kind, seq, time: '', header, ...extra },
    end: { complete: true, bytes: 10, xxh64: '', time: '' },
    body: { decoded: false, is_text: true, truncated_for_display: false, ...body }
  } as SectionView
}

describe('diff view', () => {
  beforeEach(() => {
    calls.diffs.length = 0
    calls.editors.length = 0
    fetchFullBodyText.mockReset()
    document.body.innerHTML = ''
  })

  it('compares the full client request (base) with the upstream request, JSON laid out from raw text', async () => {
    const client = section('client_request', 0, { 'Content-Type': ['application/json'], Authorization: ['Bearer x'] }, { text: 'truncated…' }, { method: 'POST', url: '/v1/messages' })
    const upstream = section('upstream_request', 1, { 'Content-Type': ['application/json'], 'x-api-key': ['sk'] }, { text: 'truncated…' }, { method: 'POST', url: 'https://up/v1/messages' })
    fetchFullBodyText.mockImplementation(async (_ref: string, kind: string) =>
      kind === 'client_request' ? '{"m":[{"t":"<a>","o":{"e":1}}]}' : '{"m":[{"t":"\\u003ca\\u003e"}]}'
    )
    const w = mount(AuditSectionPanel, {
      props: { section: upstream, kind: 'upstream_request', archiveRef: 'r', diffTargets: [{ label: '客户端请求', kind: 'client_request', section: client }] },
      global: { plugins: [i18n] },
      attachTo: document.body
    })
    await w.get('[data-testid="audit-body-diff"]').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(fetchFullBodyText).toHaveBeenCalledWith('r', 'client_request', 0)
    expect(fetchFullBodyText).toHaveBeenCalledWith('r', 'upstream_request', 0)
    const d = calls.diffs[0]
    expect(d.opts.ignoreTrimWhitespace).toBe(false)
    expect(d.opts.maxComputationTime).toBe(0)
    expect(d.model!.original.value).toContain('"t": "<a>"')
    expect(d.model!.modified.value).toContain('"t": "\\u003ca\\u003e"')
    expect(d.model!.original.language).toBe('json')
    expect(w.text()).toContain('处差异')

    // 报文头按 HTTP 文本对比
    await w.get('[data-testid="audit-header-diff"]').trigger('click')
    await flushPromises()
    const hd = calls.diffs[1]
    expect(hd.model!.original.value).toBe('POST /v1/messages HTTP/1.1\nAuthorization: Bearer x\nContent-Type: application/json')
    expect(hd.model!.modified.value).toBe('POST https://up/v1/messages HTTP/1.1\nContent-Type: application/json\nx-api-key: sk')
    w.unmount()
  })

  it('compares SSE after merging in event mode and as raw text in raw mode; base is the selected upstream response', async () => {
    const sse = (t: string) => `event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"${t}"}}\n\n`
    const up1 = section('upstream_response', 1, { 'Content-Type': ['text/event-stream'] }, {}, { status: 503 })
    const up2 = section('upstream_response', 2, { 'Content-Type': ['text/event-stream'] }, {}, { status: 200 })
    up2.index = 1
    const client = section('client_response', 0, { 'Content-Type': ['text/event-stream'] }, {
      sse: [
        { event: 'content_block_delta', data: '{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}' },
        { event: 'ping', data: '{"type":"ping"}' },
        { event: 'content_block_delta', data: '{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}' }
      ]
    }, { status: 200 })
    fetchFullBodyText.mockImplementation(async (_r: string, kind: string, index: number) =>
      kind === 'client_response' ? sse('a') + sse('b') : index === 1 ? sse('a') + sse('c') : 'error'
    )
    const w = mount(AuditSectionPanel, {
      props: {
        section: client, kind: 'client_response', archiveRef: 'r',
        diffTargets: [
          { label: '上游响应 #1', kind: 'upstream_response', section: up1 },
          { label: '上游响应 #2', kind: 'upstream_response', section: up2 }
        ]
      },
      global: { plugins: [i18n] },
      attachTo: document.body
    })
    // 事件列表：合并后 2 条（delta 合并、ping 保留）
    expect(w.findAll('[data-testid="audit-sse-events"] li')).toHaveLength(2)
    expect(w.text()).toContain('合并 ×2')

    await w.get('[data-testid="audit-body-diff"]').trigger('click')
    await flushPromises()
    await flushPromises()
    const merged = calls.diffs.at(-1)!
    expect(fetchFullBodyText).toHaveBeenCalledWith('r', 'upstream_response', 1) // 默认最后一次上游响应
    expect(merged.model!.original.value).toContain('"text": "ac"')
    expect(merged.model!.modified.value).toContain('"text": "ab"')

    await w.get('[data-testid="audit-sse-mode"]').trigger('click')
    await flushPromises()
    await flushPromises()
    const raw = calls.diffs.at(-1)!
    expect(raw.model!.modified.value).toBe(sse('a') + sse('b'))
    w.unmount()
  })
})
