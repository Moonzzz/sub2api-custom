// 差异对比用的文本整理：JSON 按原文排版（不经 JSON.parse，转义、数字写法逐字保留），
// 报文头按 HTTP 文本形式输出。
import type { ArchiveKind, SectionHeader, SectionView } from './api'

/**
 * 按原文排版 JSON：只在字符串之外的 { } [ ] , : 处插入换行与缩进，
 * 字符串与数字等字面量逐字保留（例如 < 不会被还原为 <）。
 * 不是 JSON（不以 { 或 [ 开头）时原样返回。
 */
export function prettyJSONRaw(src: string, indent = '  '): string {
  const s = src.trim()
  if (!/^[[{]/.test(s)) return src
  let out = ''
  let depth = 0
  let inStr = false
  let esc = false
  const nl = () => '\n' + indent.repeat(depth)
  for (let i = 0; i < s.length; i++) {
    const ch = s[i]
    if (inStr) {
      out += ch
      if (esc) esc = false
      else if (ch === '\\') esc = true
      else if (ch === '"') inStr = false
      continue
    }
    switch (ch) {
      case '"':
        inStr = true
        out += ch
        break
      case '{':
      case '[': {
        // 空对象 / 空数组保持在一行
        let j = i + 1
        while (j < s.length && /\s/.test(s[j])) j++
        const close = ch === '{' ? '}' : ']'
        if (s[j] === close) {
          out += ch + close
          i = j
        } else {
          depth++
          out += ch + nl()
        }
        break
      }
      case '}':
      case ']':
        depth = Math.max(0, depth - 1)
        out += nl() + ch
        break
      case ',':
        out += ',' + nl()
        break
      case ':':
        out += ': '
        break
      default:
        if (!/\s/.test(ch)) out += ch
    }
  }
  return out
}

const REASONS: Record<number, string> = {
  100: 'Continue', 101: 'Switching Protocols', 200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content',
  301: 'Moved Permanently', 302: 'Found', 304: 'Not Modified', 307: 'Temporary Redirect', 308: 'Permanent Redirect',
  400: 'Bad Request', 401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found', 405: 'Method Not Allowed',
  408: 'Request Timeout', 409: 'Conflict', 413: 'Payload Too Large', 422: 'Unprocessable Entity', 429: 'Too Many Requests',
  499: 'Client Closed Request', 500: 'Internal Server Error', 502: 'Bad Gateway', 503: 'Service Unavailable', 504: 'Gateway Timeout',
  529: 'Overloaded'
}

/**
 * 报文头的 HTTP 文本形式：请求首行 `METHOD URL PROTO`，响应首行 `PROTO STATUS REASON`；
 * 其后每行 `Name: value`，按头名（不区分大小写）排序，同名多值逐行列出。
 */
export function headersToHTTP(h: SectionHeader, response: boolean): string {
  const proto = h.proto || 'HTTP/1.1'
  const first = response ? `${proto} ${h.status ?? ''} ${REASONS[h.status ?? 0] ?? ''}`.trimEnd() : `${h.method ?? ''} ${h.url ?? ''} ${proto}`.trim()
  const rows = Object.entries(h.header ?? {})
    .sort(([a], [b]) => a.toLowerCase().localeCompare(b.toLowerCase()) || a.localeCompare(b))
    .flatMap(([k, vs]) => vs.map((v) => `${k}: ${v}`))
  return [first, ...rows].join('\n')
}

/** 判断段是否为 SSE。 */
export function isSSE(h: SectionHeader): boolean {
  const ct = Object.entries(h.header ?? {}).find(([k]) => k.toLowerCase() === 'content-type')?.[1]?.[0] ?? ''
  return h.transport === 'sse' || ct.toLowerCase().startsWith('text/event-stream')
}

/** 差异对比的基底段（请求：客户端请求；响应：上游响应）。 */
export interface DiffTarget {
  label: string
  kind: ArchiveKind
  section: SectionView
}
