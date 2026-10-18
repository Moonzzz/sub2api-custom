import { describe, expect, it } from 'vitest'
import { mergeSSE, parseSSE, renderSSE } from '@audit/sse'
import { headersToHTTP, prettyJSONRaw } from '@audit/diffText'

const ev = (event: string | undefined, data: unknown) => ({ event, data: typeof data === 'string' ? data : JSON.stringify(data) })
const parsed = (e: { data: string }) => JSON.parse(e.data)

describe('mergeSSE', () => {
  it('merges Anthropic content_block_delta across pings, keeps other events and boundaries', () => {
    const events = [
      ev('message_start', { type: 'message_start', message: { id: 'm' } }),
      ev('content_block_start', { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } }),
      ev('content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'Hel' } }),
      ev('ping', { type: 'ping' }),
      ev('content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'lo <x>' } }),
      ev('content_block_stop', { type: 'content_block_stop', index: 0 }),
      ev('content_block_start', { type: 'content_block_start', index: 1, content_block: { type: 'tool_use' } }),
      ev('content_block_delta', { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '{"a":' } }),
      ev('content_block_delta', { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '1}' } }),
      ev('content_block_stop', { type: 'content_block_stop', index: 1 }),
      ev('message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn' } }),
      ev('message_stop', { type: 'message_stop' })
    ]
    const out = mergeSSE(events)
    expect(out.map((e) => e.event)).toEqual([
      'message_start', 'content_block_start', 'content_block_delta', 'ping', 'content_block_stop',
      'content_block_start', 'content_block_delta', 'content_block_stop', 'message_delta', 'message_stop'
    ])
    expect(parsed(out[2]).delta.text).toBe('Hello <x>')
    expect(out[2].merged).toBe(2)
    expect(parsed(out[6]).delta.partial_json).toBe('{"a":1}')
  })

  it('does not merge across a content_block_stop boundary or different delta types', () => {
    const out = mergeSSE([
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'a' } }),
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'signature_delta', signature: 'S' } }),
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'b' } }),
      ev(undefined, { type: 'content_block_stop', index: 0 }),
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: 'c' } })
    ])
    expect(out).toHaveLength(4)
    expect(parsed(out[0]).delta.thinking).toBe('ab')
    expect(parsed(out[1]).delta.signature).toBe('S')
    expect(parsed(out[3]).delta.thinking).toBe('c')
  })

  it('keeps non-text Anthropic deltas (citations) untouched', () => {
    const out = mergeSSE([
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'citations_delta', citation: { x: 1 } } }),
      ev(undefined, { type: 'content_block_delta', index: 0, delta: { type: 'citations_delta', citation: { x: 2 } } })
    ])
    expect(out).toHaveLength(2)
  })

  it('merges OpenAI Responses deltas per item and keeps sequence of other events', () => {
    const out = mergeSSE([
      ev('response.created', { type: 'response.created' }),
      ev('response.output_text.delta', { type: 'response.output_text.delta', item_id: 'i1', output_index: 0, content_index: 0, delta: 'a', sequence_number: 3 }),
      ev('response.in_progress', { type: 'response.in_progress' }),
      ev('response.output_text.delta', { type: 'response.output_text.delta', item_id: 'i1', output_index: 0, content_index: 0, delta: 'b', sequence_number: 4 }),
      ev('response.function_call_arguments.delta', { type: 'response.function_call_arguments.delta', item_id: 'f1', output_index: 1, delta: '{"q"' }),
      ev('response.function_call_arguments.delta', { type: 'response.function_call_arguments.delta', item_id: 'f1', output_index: 1, delta: ':1}' }),
      ev('response.output_text.done', { type: 'response.output_text.done', item_id: 'i1' }),
      ev('response.completed', { type: 'response.completed' })
    ])
    expect(out.map((e) => e.event)).toEqual([
      'response.created', 'response.output_text.delta', 'response.in_progress', 'response.function_call_arguments.delta',
      'response.output_text.done', 'response.completed'
    ])
    expect(parsed(out[1])).toMatchObject({ delta: 'ab', sequence_number: 3 })
    expect(parsed(out[3]).delta).toBe('{"q":1}')
  })

  it('merges OpenAI Chat chunks incl. tool call arguments, stops at finish_reason and [DONE]', () => {
    const chunk = (delta: unknown, finish: string | null = null) => ev(undefined, { id: 'c', object: 'chat.completion.chunk', choices: [{ index: 0, delta, finish_reason: finish }] })
    const out = mergeSSE([
      chunk({ role: 'assistant', content: '' }),
      chunk({ content: 'Hi' }),
      chunk({ content: ' there' }),
      chunk({ tool_calls: [{ index: 0, id: 't1', type: 'function', function: { name: 'f', arguments: '{"a"' } }] }),
      chunk({ tool_calls: [{ index: 0, function: { arguments: ':1}' } }] }),
      chunk({}, 'stop'),
      ev(undefined, { id: 'c', object: 'chat.completion.chunk', choices: [], usage: { total_tokens: 5 } }),
      ev(undefined, '[DONE]')
    ])
    expect(out).toHaveLength(4)
    const d = parsed(out[0]).choices[0].delta
    expect(d.content).toBe('Hi there')
    expect(d.tool_calls[0]).toMatchObject({ id: 't1', function: { name: 'f', arguments: '{"a":1}' } })
    expect(out[0].merged).toBe(5)
    expect(out[3].data).toBe('[DONE]')
  })

  it('merges Gemini chunks and takes the latest usage', () => {
    const chunk = (text: string, extra: Record<string, unknown> = {}) =>
      ev(undefined, { candidates: [{ content: { role: 'model', parts: [{ text }] }, ...extra }], usageMetadata: { totalTokenCount: text.length } })
    const out = mergeSSE([chunk('Hel'), chunk('lo'), chunk('!', { finishReason: 'STOP' })])
    expect(out).toHaveLength(2)
    expect(parsed(out[0]).candidates[0].content.parts).toEqual([{ text: 'Hello' }])
    expect(parsed(out[0]).usageMetadata.totalTokenCount).toBe(2)
  })

  it('never drops events: non-JSON data and unknown events pass through', () => {
    const events = [ev('x', 'not json'), ev(undefined, { foo: 1 }), ev('error', { type: 'error' })]
    expect(mergeSSE(events)).toEqual(events)
  })
})

describe('text helpers', () => {
  it('parses SSE text like the backend', () => {
    expect(parseSSE('event: a\r\ndata: {"x":1}\r\n\r\ndata: [DONE]\n\n')).toEqual([{ event: 'a', data: '{"x":1}' }, { data: '[DONE]' }])
  })

  it('pretty-prints JSON without touching literals', () => {
    const src = '{"text":"\\u003cb\\u003e, {x}","n":1.0,"a":[],"o":{"k":[1,2]}}'
    expect(prettyJSONRaw(src)).toBe('{\n  "text": "\\u003cb\\u003e, {x}",\n  "n": 1.0,\n  "a": [],\n  "o": {\n    "k": [\n      1,\n      2\n    ]\n  }\n}')
    expect(prettyJSONRaw('plain text')).toBe('plain text')
  })

  it('renders headers as HTTP text sorted case-insensitively', () => {
    const req = headersToHTTP({ kind: 'client_request', seq: 0, time: '', method: 'POST', url: '/v1/messages', proto: 'HTTP/1.1', header: { 'X-B': ['2'], 'anthropic-beta': ['b1'], 'Accept': ['*/*'] } }, false)
    expect(req).toBe('POST /v1/messages HTTP/1.1\nAccept: */*\nanthropic-beta: b1\nX-B: 2')
    const resp = headersToHTTP({ kind: 'client_response', seq: 0, time: '', status: 200, header: { 'Content-Type': ['text/event-stream'] } }, true)
    expect(resp).toBe('HTTP/1.1 200 OK\nContent-Type: text/event-stream')
  })

  it('renders merged events as SSE text', () => {
    expect(renderSSE([{ event: 'e', data: '{"a":1}', merged: 2 }], prettyJSONRaw)).toBe('event: e\ndata: {\n  "a": 1\n}')
  })
})
