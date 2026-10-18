<template>
  <article class="as-card" :data-testid="`audit-section-${kind}-${section.index}`">
    <div class="as-head">
      <span v-if="isUpstream" class="as-badge as-badge--muted">{{ text.attempt }} #{{ section.seq }}</span>
      <strong v-if="h.method" class="as-break">{{ h.method }} {{ h.url }}</strong>
      <span v-if="h.status" class="as-badge" :class="h.status >= 400 ? 'as-badge--bad' : 'as-badge--ok'">{{ h.status }}</span>
      <span v-if="h.transport" class="as-badge as-badge--muted">{{ h.transport }}</span>
      <span v-if="h.proto" class="as-muted">{{ h.proto }}</span>
      <span v-if="h.replay" class="as-badge as-badge--warn">{{ text.replay }}</span>
      <span v-if="meta.account_id" class="as-muted">{{ text.account }} {{ meta.account_id }}</span>
      <span v-if="meta.ws_reused" class="as-badge as-badge--warn">{{ text.reused }}</span>
      <span class="as-muted">{{ fmtTime(h.time) }}</span>
    </div>
    <div class="as-head as-muted">
      <span>{{ section.body_bytes }} {{ text.bytes }}</span>
      <span v-if="section.hash_match === true" class="as-badge as-badge--ok" :title="section.body_sha256">{{ text.hashOk }}</span>
      <span v-else-if="section.hash_match === false" class="as-badge as-badge--bad">{{ text.hashBad }}</span>
      <span v-if="!section.end || !section.end.complete" class="as-badge as-badge--warn">
        {{ text.incompleteSection }}<template v-if="section.end?.error">: {{ section.end.error }}</template>
      </span>
      <code class="as-break" :title="'sha256'">{{ section.body_sha256.slice(0, 16) }}…</code>
    </div>

    <details v-if="headerRows.length" class="as-details">
      <summary>{{ text.headers }} ({{ headerRows.length }})</summary>
      <div class="as-table-wrap">
        <table class="as-table">
          <tbody>
            <tr v-for="([k, v], i) in headerRows" :key="i">
              <th>{{ k }}</th>
              <td class="as-break">{{ v }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </details>
    <details v-if="metaRows.length" class="as-details">
      <summary>meta ({{ metaRows.length }})</summary>
      <pre class="as-pre">{{ JSON.stringify(meta, null, 2) }}</pre>
    </details>

    <div v-if="body" class="as-body">
      <div class="as-head">
        <strong>{{ text.body }}</strong>
        <span v-if="body.content_encoding" class="as-muted">
          {{ text.encoding }}: {{ body.content_encoding }}<template v-if="body.decoded"> · {{ text.decodedForView }}</template>
        </span>
        <span v-if="body.decode_error" class="as-badge as-badge--warn">{{ body.decode_error }}</span>
        <span class="as-spacer"></span>
        <button type="button" class="as-btn" :disabled="downloading" data-testid="audit-download-raw" @click="download(false)">{{ text.downloadRaw }}</button>
        <button v-if="body.content_encoding" type="button" class="as-btn" :disabled="downloading" @click="download(true)">{{ text.downloadDecoded }}</button>
      </div>
      <p v-if="downloadMsg" class="as-note" :class="downloadOk ? 'as-note--ok' : 'as-note--bad'" data-testid="audit-download-result">{{ downloadMsg }}</p>
      <p v-if="body.truncated_for_display" class="as-note as-note--warn">{{ text.truncatedForUI }}</p>

      <template v-if="body.sse?.length">
        <div class="as-muted">
          {{ text.sseEvents }}: {{ body.sse.length }} ·
          <span :class="body.sse_terminated ? 'as-ok' : 'as-warn'">{{ body.sse_terminated ? text.sseTerminated : text.sseNotTerminated }}</span>
          <button type="button" class="as-btn as-btn--link" @click="showRawSSE = !showRawSSE">{{ showRawSSE ? text.sseEvents : text.rawText }}</button>
        </div>
        <AuditBodyView v-if="showRawSSE" :text="body.text || ''" :truncated="body.truncated_for_display" />
        <ol v-else class="as-events">
          <li v-for="(ev, i) in visibleEvents" :key="i">
            <span class="as-badge as-badge--muted">{{ ev.event || 'message' }}</span>
            <code class="as-break">{{ ev.data }}</code>
          </li>
          <li v-if="body.sse.length > visibleEvents.length">
            <button type="button" class="as-btn as-btn--link" @click="eventLimit += 500">+{{ body.sse.length - visibleEvents.length }}</button>
          </li>
        </ol>
      </template>
      <AuditBodyView v-else-if="body.is_text" :text="body.text || ''" :truncated="body.truncated_for_display" />
      <template v-else>
        <div class="as-muted">{{ text.binary }}</div>
        <AuditBodyView :text="body.base64 || ''" :truncated="body.truncated_for_display" />
      </template>
    </div>

    <div v-if="section.messages?.length" class="as-body">
      <div class="as-head">
        <strong>{{ text.wsMessages }} ({{ section.messages.length }}<template v-if="section.messages_truncated">+</template>)</strong>
      </div>
      <ol class="as-events">
        <li v-for="(m, i) in visibleMessages" :key="i">
          <span class="as-badge as-badge--muted">{{ m.dir }}</span>
          <span class="as-badge" :class="m.op === 'close' ? 'as-badge--warn' : 'as-badge--muted'">{{ m.op }}</span>
          <span class="as-muted">{{ m.size }}B<template v-if="m.compressed"> · deflate</template></span>
          <span v-if="m.error" class="as-badge as-badge--bad">{{ m.error }}</span>
          <code class="as-break">{{ m.text ?? m.base64 }}</code>
        </li>
        <li v-if="section.messages.length > visibleMessages.length">
          <button type="button" class="as-btn as-btn--link" @click="eventLimit += 500">+{{ section.messages.length - visibleMessages.length }}</button>
        </li>
      </ol>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { downloadBody, saveBlob, type ArchiveKind, type SectionView } from './api'
import AuditBodyView from './AuditBodyView.vue'
import { useAuditText } from './i18n'

const props = defineProps<{ section: SectionView; kind: ArchiveKind; archiveRef: string }>()
const text = useAuditText()

const h = computed(() => props.section.header)
const meta = computed<Record<string, any>>(() => ({ ...(h.value.meta ?? {}), ...(props.section.end?.meta ?? {}) }))
const body = computed(() => props.section.body)
const isUpstream = computed(() => props.kind === 'upstream_request' || props.kind === 'upstream_response')
const headerRows = computed<[string, string][]>(() =>
  Object.entries(h.value.header ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .flatMap(([k, vs]) => vs.map((v) => [k, v] as [string, string]))
)
const metaRows = computed(() => Object.keys(meta.value))
const showRawSSE = ref(false)
const eventLimit = ref(500)
const visibleEvents = computed(() => body.value?.sse?.slice(0, eventLimit.value) ?? [])
const visibleMessages = computed(() => props.section.messages?.slice(0, eventLimit.value) ?? [])

const downloading = ref(false)
const downloadMsg = ref('')
const downloadOk = ref(true)

async function download(decode: boolean) {
  downloading.value = true
  downloadMsg.value = ''
  try {
    const res = await downloadBody(props.archiveRef, props.kind, props.section.index, decode)
    saveBlob(res.blob, res.filename)
    if (!decode && res.expectedSha256 && res.actualSha256) {
      downloadOk.value = res.expectedSha256 === res.actualSha256
      downloadMsg.value = downloadOk.value ? `${text.value.downloadVerified} (${res.actualSha256.slice(0, 16)}…)` : text.value.downloadMismatch
    } else {
      downloadOk.value = true
      downloadMsg.value = `${text.value.downloadDone} (${res.blob.size} ${text.value.bytes})`
    }
  } catch (e) {
    downloadOk.value = false
    downloadMsg.value = (e as { message?: string })?.message ?? String(e)
  } finally {
    downloading.value = false
  }
}

function fmtTime(t?: string) {
  if (!t) return ''
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString()
}
</script>

<style scoped>
.as-card {
  border: 1px solid rgb(229 231 235);
  border-radius: 0.5rem;
  padding: 0.6rem 0.75rem;
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
  min-width: 0;
}
:global(.dark) .as-card {
  border-color: rgb(55 65 81);
}
.as-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.4rem;
}
.as-spacer {
  flex: 1;
}
.as-muted {
  color: rgb(107 114 128);
}
.as-ok {
  color: rgb(21 128 61);
}
.as-warn {
  color: rgb(180 83 9);
}
.as-break {
  word-break: break-all;
}
.as-badge {
  display: inline-block;
  border-radius: 999px;
  padding: 0 0.5rem;
  font-size: 0.7rem;
  line-height: 1.2rem;
  white-space: nowrap;
}
.as-badge--ok {
  background: rgb(220 252 231);
  color: rgb(21 128 61);
}
.as-badge--bad {
  background: rgb(254 226 226);
  color: rgb(185 28 28);
}
.as-badge--warn {
  background: rgb(254 243 199);
  color: rgb(146 64 14);
}
.as-badge--muted {
  background: rgb(243 244 246);
  color: rgb(75 85 99);
}
:global(.dark) .as-badge--muted {
  background: rgb(55 65 81);
  color: rgb(209 213 219);
}
.as-details summary {
  cursor: pointer;
  color: rgb(55 65 81);
}
:global(.dark) .as-details summary {
  color: rgb(209 213 219);
}
.as-table-wrap {
  overflow-x: auto;
}
.as-table {
  border-collapse: collapse;
  width: 100%;
}
.as-table th {
  text-align: left;
  font-weight: 500;
  white-space: nowrap;
  padding: 0.15rem 0.6rem 0.15rem 0;
  color: rgb(107 114 128);
  vertical-align: top;
}
.as-table td {
  padding: 0.15rem 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.as-pre {
  margin: 0;
  max-height: 28rem;
  overflow: auto;
  padding: 0.5rem;
  border-radius: 0.375rem;
  background: rgb(249 250 251);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.75rem;
  white-space: pre-wrap;
  word-break: break-all;
}
:global(.dark) .as-pre {
  background: rgb(31 41 55);
}
.as-body {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}
.as-btn {
  border: 1px solid rgb(209 213 219);
  border-radius: 0.375rem;
  padding: 0 0.5rem;
  font-size: 0.75rem;
  line-height: 1.4rem;
  background: transparent;
  color: inherit;
  cursor: pointer;
}
.as-btn--link {
  border: none;
  color: rgb(37 99 235);
}
.as-btn:disabled {
  opacity: 0.5;
}
.as-note {
  margin: 0;
  font-size: 0.75rem;
}
.as-note--ok {
  color: rgb(21 128 61);
}
.as-note--bad {
  color: rgb(185 28 28);
}
.as-note--warn {
  color: rgb(146 64 14);
}
.as-events {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 28rem;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
  font-size: 0.75rem;
}
.as-events li {
  display: flex;
  flex-wrap: wrap;
  gap: 0.35rem;
  align-items: baseline;
}
</style>
