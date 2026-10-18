<template>
  <Teleport to="body">
    <div class="aa-overlay" data-testid="audit-archive-modal" @click.self="emit('close')">
      <div class="aa-dialog" role="dialog" aria-modal="true" :aria-label="text.title">
        <header class="aa-head">
          <div class="aa-head__title">
            <strong>{{ text.title }}</strong>
            <span class="aa-muted">· {{ text.usageId }} #{{ usageId }}</span>
          </div>
          <button type="button" class="aa-btn" @click="emit('close')">{{ text.close }}</button>
        </header>

        <div class="aa-body">
          <div v-if="loading" class="aa-state">{{ text.loading }}</div>

          <div v-else-if="error" class="aa-state aa-state--error" data-testid="audit-archive-error">
            <p>{{ error }}</p>
            <button type="button" class="aa-btn" @click="load(currentRef)">{{ text.retry }}</button>
          </div>

          <template v-else-if="data">
            <div v-if="data.resolution === 'not_found'" class="aa-state aa-state--warn" data-testid="audit-archive-not-found">
              {{ text.notFound }}
              <div class="aa-kv aa-kv--compact">
                <span>{{ text.usageRequestId }}</span><code>{{ data.usage.request_id }}</code>
              </div>
            </div>

            <div v-if="data.resolution === 'ambiguous' || data.resolution === 'best_match_with_alternatives'" class="aa-state aa-state--warn" data-testid="audit-archive-candidates">
              <p>{{ data.resolution === 'ambiguous' ? text.ambiguous : text.alternatives }}</p>
              <ul class="aa-cands">
                <li v-for="c in data.candidates" :key="c.ref">
                  <code>{{ c.ref }}</code>
                  <span class="aa-muted">score {{ c.score }} · {{ c.match_reason.join(', ') || '-' }}</span>
                  <span v-if="c.ref === data.ref" class="aa-badge aa-badge--ok">{{ text.current }}</span>
                  <button v-else type="button" class="aa-btn aa-btn--sm" @click="load(c.ref)">{{ text.choose }}</button>
                </li>
              </ul>
            </div>

            <template v-if="data.archive">
              <section class="aa-card">
                <div class="aa-row">
                  <span class="aa-badge" :class="statusClass(data.archive.status)" data-testid="audit-archive-status">{{ statusText(data.archive.status) }}</span>
                  <code class="aa-muted aa-break">{{ data.archive.ref }}</code>
                </div>
                <details v-if="data.archive.issues?.length" class="aa-issues" open>
                  <summary>{{ text.issues }} ({{ data.archive.issues.length }})</summary>
                  <ul>
                    <li v-for="(issue, i) in data.archive.issues" :key="i">{{ issue }}</li>
                  </ul>
                </details>
              </section>

              <section class="aa-card">
                <h4>{{ text.summary }}</h4>
                <div class="aa-kv">
                  <span>{{ text.usageId }}</span><code>#{{ data.usage.id }}</code>
                  <span>{{ text.usageRequestId }}</span><code class="aa-break">{{ data.usage.request_id }}</code>
                  <span>{{ text.clientRequestId }}</span><code class="aa-break">{{ s.request_id ?? '-' }}</code>
                  <span>{{ text.session }}</span><code class="aa-break">{{ s.session_id || 'nosession' }}<template v-if="s.session_header"> ({{ s.session_header }})</template></code>
                  <span>{{ text.trace }}</span><code class="aa-break">{{ s.trace?.trace_id ?? '-' }} / {{ s.trace?.span_id ?? '-' }} <span class="aa-muted">{{ s.trace?.source }}</span></code>
                  <span>{{ text.apiKey }}</span><code>{{ s.api_key_id ?? '-' }} <span class="aa-muted">(usage: {{ data.usage.api_key_id }})</span></code>
                  <span>{{ text.user }}</span><code>{{ s.user_id ?? '-' }}</code>
                  <span>{{ text.account }}</span><code>{{ s.account_id ?? '-' }} <span class="aa-muted">(usage: {{ data.usage.account_id }})</span></code>
                  <span>{{ text.clientStatus }}</span><code>{{ s.client_status ?? s.status ?? '-' }}</code>
                  <span>{{ text.duration }}</span><code>{{ s.duration_ms != null ? s.duration_ms + ' ms' : '-' }}</code>
                </div>
                <div v-if="usageNotes.length" class="aa-notes">
                  <div class="aa-muted">{{ text.usageCallbacks }} — {{ text.usageCallbackHint }}</div>
                  <div v-for="(n, i) in usageNotes" :key="i" class="aa-note">
                    <code>{{ n.usage_request_id }}</code>
                    <span class="aa-badge" :class="n.usage_state === 'write_failed' ? 'aa-badge--bad' : 'aa-badge--muted'">{{ n.usage_state }}</span>
                    <span v-if="n.usage_error" class="aa-muted">{{ n.usage_error }}</span>
                  </div>
                </div>
              </section>

              <section v-if="data.archive.attempts.length" class="aa-card">
                <h4>{{ text.attempts }} ({{ data.archive.attempts.length }})</h4>
                <div class="aa-table-wrap">
                  <table class="aa-table" data-testid="audit-archive-attempts">
                    <thead>
                      <tr><th>#</th><th>{{ text.account }}</th><th>Status</th><th>URL</th><th>Transport</th><th>Span</th><th></th></tr>
                    </thead>
                    <tbody>
                      <tr v-for="a in data.archive.attempts" :key="a.seq">
                        <td>{{ a.seq }}</td>
                        <td>{{ a.account_id ?? '-' }}</td>
                        <td><span class="aa-badge" :class="(a.status ?? 0) >= 400 || !a.status ? 'aa-badge--bad' : 'aa-badge--ok'">{{ a.status ?? '-' }}</span></td>
                        <td class="aa-break">{{ a.method }} {{ a.url }}</td>
                        <td>{{ a.transport }}<span v-if="a.ws_reused" class="aa-muted"> · {{ text.reused }}</span></td>
                        <td><code>{{ a.span_id ?? '-' }}</code></td>
                        <td>
                          <span v-if="!a.complete" class="aa-badge aa-badge--warn" :title="a.error">{{ text.incompleteSection }}</span>
                        </td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              </section>

              <nav class="aa-tabs" role="tablist">
                <button
                  v-for="f in data.archive.files"
                  :key="f.kind"
                  type="button"
                  role="tab"
                  class="aa-tab"
                  :class="{ 'aa-tab--active': activeKind === f.kind }"
                  :data-testid="`audit-tab-${f.kind}`"
                  @click="activeKind = f.kind"
                >
                  {{ text.files[f.kind] }}
                  <span class="aa-muted">({{ f.sections.length }})</span>
                  <span v-if="f.status !== 'ok'" class="aa-dot" :title="text.fileStatus[f.status]"></span>
                </button>
              </nav>

              <section v-if="activeFile" class="aa-file" :data-testid="`audit-file-${activeFile.kind}`">
                <div class="aa-row aa-muted">
                  <span class="aa-badge" :class="activeFile.status === 'ok' ? 'aa-badge--ok' : 'aa-badge--bad'">{{ text.fileStatus[activeFile.status] }}</span>
                  <span>{{ formatBytes(activeFile.raw_size) }}</span>
                  <span v-if="data.archive.storage" data-testid="audit-storage">
                    {{ text.storage[data.archive.storage.location] }} ·
                    {{ text.requestSize }} zstd {{ formatBytes(data.archive.storage.compressed_size) }} → {{ formatBytes(data.archive.storage.raw_size) }}
                    <template v-if="data.archive.storage.session_compressed_size">
                      · {{ text.sessionSize }} {{ formatBytes(data.archive.storage.session_compressed_size) }}
                    </template>
                  </span>
                  <span v-if="activeFile.error">{{ activeFile.error }}</span>
                </div>
                <div v-if="!activeFile.sections.length" class="aa-state">{{ text.noSections }}</div>
                <AuditSectionPanel
                  v-for="sec in activeFile.sections"
                  :key="`${activeFile.kind}-${sec.index}`"
                  :section="sec"
                  :kind="activeFile.kind"
                  :archive-ref="data.archive.ref"
                />
              </section>
            </template>
          </template>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import AuditSectionPanel from './AuditSectionPanel.vue'
import { fetchUsageArchive, type ApiError, type ArchiveKind, type UsageArchiveResponse } from './api'
import { formatBytes, useAuditText } from './i18n'

const props = defineProps<{ usageId: number }>()
const emit = defineEmits<{ (e: 'close'): void }>()
const text = useAuditText()
const route = useRoute()

const loading = ref(false)
const error = ref('')
const data = ref<UsageArchiveResponse | null>(null)
const activeKind = ref<ArchiveKind>('client_request')
const currentRef = ref<string | undefined>(undefined)
let controller: AbortController | null = null
let generation = 0

const s = computed<Record<string, any>>(() => data.value?.archive?.summary ?? {})
const usageNotes = computed<Record<string, any>[]>(() => (s.value.usage_notes as Record<string, any>[] | undefined) ?? [])
const activeFile = computed(() => data.value?.archive?.files.find((f) => f.kind === activeKind.value))

function statusText(st: string) {
  return (
    { complete: text.value.statusComplete, incomplete: text.value.statusIncomplete, in_progress: text.value.statusInProgress, missing: text.value.statusMissing } as Record<string, string>
  )[st] ?? st
}
function statusClass(st: string) {
  return st === 'complete' ? 'aa-badge--ok' : st === 'in_progress' ? 'aa-badge--warn' : 'aa-badge--bad'
}

function describeError(e: ApiError): string {
  if (e.status === 401 || e.status === 403) return text.value.errForbidden
  if (e.status === 503 || e.reason === 'AUDIT_ARCHIVE_DISABLED') return text.value.errDisabled
  if (e.status === 404 || e.reason === 'USAGE_NOT_FOUND') return text.value.errUsageMissing
  return `${text.value.errGeneric}: ${e.message ?? String(e)}`
}

async function load(ref?: string) {
  controller?.abort()
  controller = new AbortController()
  const gen = ++generation
  const requestedUsage = props.usageId
  loading.value = true
  error.value = ''
  try {
    const resp = await fetchUsageArchive(requestedUsage, ref, controller.signal)
    // 只接受与当前弹窗、当前用量记录一致的最新响应，防止快速切换时串日志。
    if (gen !== generation || resp.usage?.id !== props.usageId) return
    data.value = resp
    currentRef.value = resp.ref
  } catch (e) {
    if (gen !== generation) return
    const err = e as ApiError & { code?: string }
    if (err?.code === 'ERR_CANCELED') return
    error.value = describeError(err)
    data.value = null
  } finally {
    if (gen === generation) loading.value = false
  }
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}

onMounted(() => {
  document.addEventListener('keydown', onKey)
  load()
})
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKey)
  controller?.abort()
})
// 路由跳转（含分页参数变化）时关闭，避免弹窗内容与页面不一致。
watch(() => route.fullPath, () => emit('close'))
</script>

<style scoped>
.aa-overlay {
  position: fixed;
  inset: 0;
  z-index: 1000;
  background: rgb(15 23 42 / 0.55);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 1rem;
  text-align: left;
}
.aa-dialog {
  width: min(1100px, 100%);
  max-height: calc(100vh - 2rem);
  display: flex;
  flex-direction: column;
  border-radius: 0.75rem;
  background: #fff;
  color: rgb(17 24 39);
  box-shadow: 0 20px 50px rgb(0 0 0 / 0.3);
  font-size: 0.8125rem;
}
:global(.dark) .aa-dialog {
  background: rgb(17 24 39);
  color: rgb(229 231 235);
}
.aa-head {
  flex-shrink: 0;
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 0.5rem;
  padding: 0.75rem 1rem;
  border-bottom: 1px solid rgb(229 231 235);
}
:global(.dark) .aa-head {
  border-color: rgb(55 65 81);
}
.aa-head__title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.aa-body {
  overflow: auto;
  padding: 0.75rem 1rem 1rem;
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
/* 纵向 flex 中的子块（尤其带 overflow 的标签栏）不允许被压缩，否则会塌成 1px。 */
.aa-body > * {
  flex-shrink: 0;
}
.aa-card {
  border: 1px solid rgb(229 231 235);
  border-radius: 0.5rem;
  padding: 0.6rem 0.75rem;
}
:global(.dark) .aa-card {
  border-color: rgb(55 65 81);
}
.aa-card h4 {
  margin: 0 0 0.4rem;
  font-weight: 600;
}
.aa-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.aa-kv {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 0.25rem 0.75rem;
}
.aa-kv > span {
  color: rgb(107 114 128);
}
.aa-kv--compact {
  margin-top: 0.5rem;
}
.aa-muted {
  color: rgb(107 114 128);
}
.aa-break {
  word-break: break-all;
}
.aa-state {
  padding: 1rem;
  border-radius: 0.5rem;
  background: rgb(249 250 251);
}
:global(.dark) .aa-state {
  background: rgb(31 41 55);
}
.aa-state--error {
  background: rgb(254 242 242);
  color: rgb(185 28 28);
}
.aa-state--warn {
  background: rgb(255 251 235);
  color: rgb(146 64 14);
}
:global(.dark) .aa-state--error,
:global(.dark) .aa-state--warn {
  background: rgb(55 48 30);
}
.aa-cands {
  list-style: none;
  padding: 0;
  margin: 0.5rem 0 0;
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}
.aa-cands li {
  display: flex;
  flex-wrap: wrap;
  gap: 0.5rem;
  align-items: center;
  word-break: break-all;
}
.aa-btn {
  border: 1px solid rgb(209 213 219);
  border-radius: 0.375rem;
  padding: 0.2rem 0.6rem;
  background: transparent;
  color: inherit;
  cursor: pointer;
}
.aa-btn--sm {
  padding: 0 0.45rem;
  font-size: 0.75rem;
}
.aa-badge {
  display: inline-block;
  border-radius: 999px;
  padding: 0 0.5rem;
  font-size: 0.7rem;
  line-height: 1.2rem;
  white-space: nowrap;
}
.aa-badge--ok {
  background: rgb(220 252 231);
  color: rgb(21 128 61);
}
.aa-badge--bad {
  background: rgb(254 226 226);
  color: rgb(185 28 28);
}
.aa-badge--warn {
  background: rgb(254 243 199);
  color: rgb(146 64 14);
}
.aa-badge--muted {
  background: rgb(243 244 246);
  color: rgb(75 85 99);
}
.aa-issues ul {
  margin: 0.25rem 0 0;
  padding-left: 1.2rem;
}
.aa-notes {
  margin-top: 0.5rem;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.aa-note {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
  align-items: center;
}
.aa-table-wrap {
  overflow-x: auto;
}
.aa-table {
  width: 100%;
  border-collapse: collapse;
}
.aa-table th,
.aa-table td {
  text-align: left;
  padding: 0.3rem 0.4rem;
  border-bottom: 1px solid rgb(243 244 246);
  vertical-align: top;
}
:global(.dark) .aa-table th,
:global(.dark) .aa-table td {
  border-color: rgb(55 65 81);
}
.aa-tabs {
  display: flex;
  gap: 0.25rem;
  overflow-x: auto;
  border-bottom: 1px solid rgb(229 231 235);
}
.aa-tab {
  border: none;
  background: transparent;
  color: inherit;
  padding: 0.45rem 0.7rem;
  cursor: pointer;
  white-space: nowrap;
  border-bottom: 2px solid transparent;
  position: relative;
}
.aa-tab--active {
  border-bottom-color: rgb(37 99 235);
  font-weight: 600;
}
.aa-dot {
  display: inline-block;
  width: 0.45rem;
  height: 0.45rem;
  border-radius: 999px;
  background: rgb(220 38 38);
  margin-left: 0.25rem;
}
.aa-file {
  display: flex;
  flex-direction: column;
  gap: 0.6rem;
}
@media (max-width: 640px) {
  .aa-overlay {
    padding: 0;
  }
  .aa-dialog {
    max-height: 100vh;
    height: 100vh;
    border-radius: 0;
  }
  .aa-kv {
    grid-template-columns: minmax(0, 1fr);
  }
  .aa-kv > span {
    margin-top: 0.25rem;
  }
}
</style>
