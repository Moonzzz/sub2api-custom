// SSE 解析与增量事件合并（仅用于「SSE 事件」展示与差异对比；原文与下载不受影响）。
//
// 合并规则：
//   - 各协议的增量事件，只要没遇到开始 / 结束事件，就把同一内容块的相邻增量合并为一条；
//   - 不同内容块（Anthropic index、Responses item/content、Chat choice、Gemini candidate）分别合并；
//   - 夹在中间的其它事件（ping、message_delta、response.in_progress…）原样保留、不丢；
//     合并后的事件位于该组第一个增量的位置，被跨过的事件按原顺序排在其后；
//   - 只做无损合并：文本类字段拼接，相同值保留，遇到无法无损合并的字段则另起一组。
import type { SSEEvent } from './api'

export interface MergedSSEEvent extends SSEEvent {
  /** 合并的原始事件数（>1 表示合并而来） */
  merged?: number
}

/** 解析 SSE 文本（与后端 parseSSE 一致：空行分隔，event / id / data 行）。 */
export function parseSSE(text: string): SSEEvent[] {
  const out: SSEEvent[] = []
  for (const block of text.replace(/\r\n/g, '\n').split('\n\n')) {
    if (!block.trim()) continue
    const ev: SSEEvent = { data: '' }
    const data: string[] = []
    for (const line of block.split('\n')) {
      if (line.startsWith('event:')) ev.event = line.slice(6).trim()
      else if (line.startsWith('id:')) ev.id = line.slice(3).trim()
      else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
    }
    ev.data = data.join('\n')
    out.push(ev)
  }
  return out
}

type Json = any // eslint-disable-line @typescript-eslint/no-explicit-any

/** 文本类字段：增量拼接。 */
const CONCAT = new Set(['text', 'partial_json', 'thinking', 'signature', 'delta', 'content', 'reasoning_content', 'reasoning', 'refusal', 'arguments'])
/** 逐条变化、只是序号 / 填充的字段：保留第一条。 */
const KEEP_FIRST = new Set(['sequence_number', 'obfuscation'])
/** 累计型统计：取最新一条。 */
const TAKE_LATEST = new Set(['usage', 'usageMetadata'])
/** Anthropic 可无损拼接的增量类型。 */
const ANTHROPIC_TEXT_DELTAS = new Set(['text_delta', 'input_json_delta', 'thinking_delta', 'signature_delta'])

const isObj = (v: unknown): v is Record<string, Json> => typeof v === 'object' && v !== null && !Array.isArray(v)
const clone = <T>(v: T): T => (v === undefined ? v : JSON.parse(JSON.stringify(v)))
const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)

/** 把 src 合并进 t（原地）；无法无损合并时返回 false（调用方需在副本上执行）。 */
function mergeInto(t: Record<string, Json>, s: Record<string, Json>): boolean {
  for (const [k, sv] of Object.entries(s)) {
    const tv = t[k]
    if (tv === undefined) {
      t[k] = clone(sv)
    } else if (typeof sv === 'string' && typeof tv === 'string' && CONCAT.has(k)) {
      t[k] = tv + sv
    } else if (same(tv, sv) || KEEP_FIRST.has(k)) {
      // 保留
    } else if (TAKE_LATEST.has(k)) {
      t[k] = clone(sv)
    } else if (isObj(tv) && isObj(sv)) {
      if (!mergeInto(tv, sv)) return false
    } else if (Array.isArray(tv) && Array.isArray(sv)) {
      if (!mergeArray(k, tv, sv)) return false
    } else {
      return false
    }
  }
  return true
}

function mergeArray(key: string, t: Json[], s: Json[]): boolean {
  if (key === 'parts') {
    // Gemini：文本 part 拼进最后一个同类文本 part，其它 part 追加
    for (const p of s) {
      const last = t[t.length - 1]
      const textOnly = (x: Json) => isObj(x) && typeof x.text === 'string' && Object.keys(x).every((k) => k === 'text' || k === 'thought')
      if (textOnly(p) && textOnly(last) && !!last.thought === !!p.thought) last.text += p.text
      else t.push(clone(p))
    }
    return true
  }
  const keyed = s.every((x) => isObj(x) && typeof x.index === 'number')
  for (let i = 0; i < s.length; i++) {
    const sv = s[i]
    const tv = keyed ? t.find((x) => isObj(x) && x.index === sv.index) : t[i]
    if (tv === undefined) t.push(clone(sv))
    else if (isObj(tv) && isObj(sv)) {
      if (!mergeInto(tv, sv)) return false
    } else if (!same(tv, sv)) return false
  }
  return true
}

type Classified = { kind: 'delta'; key: string } | { kind: 'boundary' } | { kind: 'other' }

function indexes(arr: Json[]): string {
  return arr.map((x, i) => (isObj(x) && typeof x.index === 'number' ? x.index : i)).join(',')
}

/** 判断事件类型：增量（可合并）/ 开始结束边界 / 其它。 */
export function classifySSE(ev: SSEEvent, obj: Json): Classified {
  if (ev.data.trim() === '[DONE]') return { kind: 'boundary' }
  if (!isObj(obj)) return { kind: 'other' }
  const type: string = typeof obj.type === 'string' ? obj.type : ev.event ?? ''

  // Anthropic Messages
  if (type === 'content_block_delta') {
    if (isObj(obj.delta) && ANTHROPIC_TEXT_DELTAS.has(obj.delta.type)) return { kind: 'delta', key: `anthropic:${obj.index}:${obj.delta.type}` }
    return { kind: 'other' }
  }
  if (['message_start', 'message_stop', 'content_block_start', 'content_block_stop'].includes(type)) return { kind: 'boundary' }

  // OpenAI Responses
  if (type.startsWith('response.')) {
    if (type.endsWith('.delta') && typeof obj.delta === 'string') {
      const part = obj.content_index ?? obj.summary_index ?? ''
      return { kind: 'delta', key: `responses:${type}:${obj.item_id ?? ''}:${obj.output_index ?? ''}:${part}` }
    }
    if (['response.created', 'response.completed', 'response.failed', 'response.incomplete', 'response.done'].includes(type) || type.endsWith('.added') || type.endsWith('.done')) {
      return { kind: 'boundary' }
    }
    return { kind: 'other' }
  }

  // OpenAI Chat Completions
  if (Array.isArray(obj.choices)) {
    if (obj.choices.some((c: Json) => isObj(c) && c.finish_reason != null)) return { kind: 'boundary' }
    if (obj.choices.length && obj.choices.every((c: Json) => isObj(c) && isObj(c.delta))) return { kind: 'delta', key: `chat:${indexes(obj.choices)}` }
    return { kind: 'other' }
  }

  // Gemini
  if (Array.isArray(obj.candidates)) {
    if (obj.candidates.some((c: Json) => isObj(c) && c.finishReason != null)) return { kind: 'boundary' }
    if (obj.candidates.length) return { kind: 'delta', key: `gemini:${indexes(obj.candidates)}` }
  }
  return { kind: 'other' }
}

function parseData(data: string): Json {
  try {
    return JSON.parse(data)
  } catch {
    return undefined
  }
}

/** 合并相邻增量事件；其余事件原样保留。 */
export function mergeSSE(events: SSEEvent[]): MergedSSEEvent[] {
  const out: MergedSSEEvent[] = []
  // 当前未结束的内容块：key → 输出中的位置与合并对象
  const open = new Map<string, { at: number; obj: Json }>()
  for (const ev of events) {
    const obj = parseData(ev.data)
    const c = classifySSE(ev, obj)
    if (c.kind === 'boundary') {
      open.clear()
      out.push({ ...ev })
      continue
    }
    if (c.kind === 'other') {
      out.push({ ...ev })
      continue
    }
    const cur = open.get(c.key)
    if (cur) {
      const next = clone(cur.obj)
      if (mergeInto(next, obj)) {
        cur.obj = next
        const target = out[cur.at]
        target.data = JSON.stringify(next)
        target.merged = (target.merged ?? 1) + 1
        continue
      }
    }
    open.set(c.key, { at: out.length, obj })
    out.push({ ...ev })
  }
  return out
}

/** 把事件序列渲染为 SSE 文本（用于差异对比）：data 为 JSON 时按原文排版。 */
export function renderSSE(events: MergedSSEEvent[], pretty: (s: string) => string): string {
  return events
    .map((ev) => {
      const lines: string[] = []
      if (ev.event) lines.push(`event: ${ev.event}`)
      if (ev.id) lines.push(`id: ${ev.id}`)
      lines.push(`data: ${pretty(ev.data)}`)
      return lines.join('\n')
    })
    .join('\n\n')
}
