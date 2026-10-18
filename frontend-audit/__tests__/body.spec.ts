import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'

vi.mock('../src/monaco', () => import('./fakeMonaco'))
import { calls } from './fakeMonaco'
import AuditBodyView from '../src/AuditBodyView.vue'

const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: {} } })
const view = (props: Record<string, unknown>) => mount(AuditBodyView, { props, global: { plugins: [i18n] }, attachTo: document.body })

describe('body view (Monaco)', () => {
  beforeEach(() => {
    calls.options.length = 0
    calls.actions.length = 0
    calls.editors.length = 0
  })

  it('passes raw JSON text to the editor and formats it with the built-in formatter', async () => {
    const w = view({ text: '{"a":{"b":[1,2]}}' })
    await flushPromises()
    const ed = calls.editors[0]
    expect(ed.opts.language).toBe('json')
    expect(ed.opts.readOnly).toBe(true)
    expect(ed.opts.folding).toBe(true)
    expect(ed.opts.wordWrap).toBe('off')
    expect(calls.actions).toContain('editor.action.formatDocument')
    expect(ed.model.getLineCount()).toBeGreaterThan(1)
    expect(ed.readOnly).toBe(true)
    w.unmount()
  })

  it('shows non-JSON as plain text without formatting', async () => {
    const w = view({ text: 'event: ping\ndata: {}\n' })
    await flushPromises()
    expect(calls.editors[0].opts.language).toBe('plaintext')
    expect(calls.actions).not.toContain('editor.action.formatDocument')
    expect(w.find('[data-testid="audit-body-fold"]').exists()).toBe(false)
    w.unmount()
  })

  it('toggles line wrapping and runs fold / unfold all', async () => {
    const w = view({ text: '{"a":[1]}' })
    await flushPromises()
    await w.get('[data-testid="audit-body-wrap"]').trigger('click')
    expect(calls.options).toContainEqual({ wordWrap: 'on' })
    await w.get('[data-testid="audit-body-wrap"]').trigger('click')
    expect(calls.options).toContainEqual({ wordWrap: 'off' })
    await w.get('[data-testid="audit-body-fold"]').trigger('click')
    await w.get('[data-testid="audit-body-unfold"]').trigger('click')
    expect(calls.actions).toEqual(expect.arrayContaining(['editor.foldAll', 'editor.unfoldAll']))
    w.unmount()
  })

  it('copies the raw text, not the formatted view', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    Object.defineProperty(navigator, 'permissions', { value: undefined, configurable: true })
    const raw = '{"n":1.0,"id":12345678901234567890}'
    const w = view({ text: raw, truncated: true })
    await flushPromises()
    await w.get('[data-testid="audit-body-copy"]').trigger('click')
    await flushPromises()
    const msg = w.get('[data-testid="audit-body-copy-result"]').text()
    expect(msg).toContain('已复制')
    expect(msg).toContain('截断')
    w.unmount()
  })
})
