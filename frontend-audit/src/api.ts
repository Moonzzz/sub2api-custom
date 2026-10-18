import { apiClient } from '@/api/client'

export type ArchiveKind = 'client_request' | 'upstream_request' | 'upstream_response' | 'client_response'

export interface SectionHeader {
  kind: ArchiveKind
  seq: number
  time: string
  method?: string
  url?: string
  proto?: string
  host?: string
  remote_addr?: string
  status?: number
  header?: Record<string, string[]>
  transport?: string
  replay?: boolean
  meta?: Record<string, unknown>
}

export interface SectionEnd {
  time: string
  bytes: number
  xxh64: string
  complete: boolean
  error?: string
  meta?: Record<string, unknown>
  raw_in?: number
  messages?: number
}

export interface SSEEvent {
  event?: string
  id?: string
  data: string
}

export interface BodyView {
  content_encoding?: string
  decoded: boolean
  decode_error?: string
  is_text: boolean
  text?: string
  base64?: string
  truncated_for_display: boolean
  sse?: SSEEvent[]
  sse_terminated?: boolean
}

export interface WSMessage {
  dir: string
  op: string
  time?: string
  size: number
  compressed?: boolean
  text?: string
  base64?: string
  error?: string
}

export interface SectionView {
  index: number
  seq: number
  header: SectionHeader
  end?: SectionEnd
  complete: boolean
  body_bytes: number
  body_sha256: string
  hash_match?: boolean
  body?: BodyView
  messages?: WSMessage[]
  messages_truncated?: boolean
  raw_bytes?: number
}

export interface FileView {
  kind: ArchiveKind
  exists: boolean
  compressed_size: number
  raw_size: number
  status: 'ok' | 'missing' | 'empty' | 'truncated' | 'corrupt' | 'error'
  error?: string
  sections: SectionView[]
  notes?: Record<string, unknown>[]
}

export interface AttemptView {
  seq: number
  account_id?: number
  method?: string
  url?: string
  transport?: string
  status?: number
  span_id?: string
  error?: string
  complete: boolean
  ws_reused?: boolean
  request_sections: number[]
  response_sections: number[]
}

export interface StorageInfo {
  location: 'active' | 'archive' | 'missing'
  status: string
  error?: string
  compressed_size: number
  session_compressed_size?: number
  raw_size: number
}

export interface ArchiveView {
  ref: string
  storage?: StorageInfo
  files: FileView[]
  summary: Record<string, any>
  attempts: AttemptView[]
  status: 'complete' | 'incomplete' | 'in_progress' | 'missing'
  issues?: string[]
}

export interface IndexEntry {
  ref: string
  t: string
  via: string
  request_id?: string
  session?: string
  usage_request_id?: string
  usage_state?: string
  usage_error?: string
  api_key_id?: number
  account_id?: number
}

export interface Candidate {
  ref: string
  entries: IndexEntry[]
  score: number
  match_reason: string[]
}

export interface UsageRef {
  id: number
  request_id: string
  api_key_id: number
  account_id: number
  user_id: number
  created_at: string
}

export type Resolution = 'unique' | 'chosen' | 'not_found' | 'ambiguous' | 'best_match_with_alternatives'

export interface UsageArchiveResponse {
  usage: UsageRef
  candidates: Candidate[]
  resolution: Resolution
  ref?: string
  archive?: ArchiveView
}

export interface ApiError {
  status?: number
  code?: string | number
  message?: string
  reason?: string
}

const BASE = '/admin/audit-archive'

export async function fetchUsageArchive(usageId: number, ref: string | undefined, signal: AbortSignal): Promise<UsageArchiveResponse> {
  const { data } = await apiClient.get<UsageArchiveResponse>(`${BASE}/usage/${usageId}`, {
    params: ref ? { ref } : undefined,
    signal,
    timeout: 120000
  })
  return data
}

export interface DownloadResult {
  blob: Blob
  filename: string
  expectedSha256?: string
  actualSha256?: string
}

async function sha256Hex(blob: Blob): Promise<string | undefined> {
  if (!globalThis.crypto?.subtle) return undefined
  const digest = await globalThis.crypto.subtle.digest('SHA-256', await blob.arrayBuffer())
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}

/** 下载某段正文；raw 下载在浏览器端复核 SHA-256 与归档记录一致。 */
export async function downloadBody(ref: string, kind: ArchiveKind, index: number, decode: boolean): Promise<DownloadResult> {
  const resp = await apiClient.get<Blob>(`${BASE}/body`, {
    params: { ref, kind, index, decode: decode ? 1 : 0 },
    responseType: 'blob',
    timeout: 0
  })
  const blob = resp.data
  const disposition = String(resp.headers['content-disposition'] || '')
  const match = /filename="?([^";]+)"?/.exec(disposition)
  const filename = match?.[1] || `${ref.split('/').pop()}-${kind}-${index}${decode ? '-decoded' : ''}.bin`
  const expectedSha256 = (resp.headers['x-archive-body-sha256'] as string | undefined) || undefined
  const actualSha256 = !decode ? await sha256Hex(blob) : undefined
  return { blob, filename, expectedSha256, actualSha256 }
}

// 差异对比用的完整正文（按 Content-Encoding 解码），按归档引用缓存，避免重复下载。
const fullTextCache = new Map<string, Promise<string>>()

/** 读取某段的完整正文文本（不受展示上限截断）。 */
export function fetchFullBodyText(ref: string, kind: ArchiveKind, index: number): Promise<string> {
  const key = `${ref}|${kind}|${index}`
  let p = fullTextCache.get(key)
  if (!p) {
    p = apiClient
      .get<Blob>(`${BASE}/body`, { params: { ref, kind, index, decode: 1 }, responseType: 'blob', timeout: 0 })
      .then((resp) => resp.data.text())
    p.catch(() => fullTextCache.delete(key))
    if (fullTextCache.size > 32) fullTextCache.delete(fullTextCache.keys().next().value as string)
    fullTextCache.set(key, p)
  }
  return p
}

/** 下载整个请求：zip 内为该请求的存档数据原样（单个 .zst）。 */
export async function downloadBundle(ref: string): Promise<DownloadResult> {
  const resp = await apiClient.get<Blob>(`${BASE}/bundle`, { params: { ref }, responseType: 'blob', timeout: 0 })
  const disposition = String(resp.headers['content-disposition'] || '')
  const match = /filename="?([^";]+)"?/.exec(disposition)
  return { blob: resp.data, filename: match?.[1] || `${ref.split('/').pop()}.zip` }
}

export function saveBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
