<template>
  <div class="ad" data-testid="audit-diff">
    <div class="ad-bar">
      <span class="ad-label"><span class="ad-tag ad-tag--base">{{ text.diffBase }}</span> {{ originalLabel }}</span>
      <span class="ad-label"><span class="ad-tag">{{ text.diffTarget }}</span> {{ modifiedLabel }}</span>
      <span class="ad-spacer"></span>
      <span v-if="loadError" class="ad-bad">{{ loadError }}</span>
      <span v-else-if="changes !== null" class="ad-muted" data-testid="audit-diff-changes">{{ changes === 0 ? text.diffNone : `${changes} ${text.diffChanges}` }}</span>
      <button type="button" class="ad-btn" :class="{ 'ad-btn--on': sideBySide }" data-testid="audit-diff-sbs" @click="sideBySide = !sideBySide">
        {{ sideBySide ? text.diffSideBySide : text.diffInline }}
      </button>
      <button type="button" class="ad-btn" :class="{ 'ad-btn--on': hideUnchanged }" @click="hideUnchanged = !hideUnchanged">{{ text.diffHideUnchanged }}</button>
      <button type="button" class="ad-btn" :class="{ 'ad-btn--on': wrap }" @click="wrap = !wrap">{{ text.wrapLines }}</button>
    </div>
    <p v-if="note" class="ad-note">{{ note }}</p>
    <div ref="host" class="ad-host" :style="{ height: `${height}px` }"></div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useMutationObserver } from '@vueuse/core'
import type * as Monaco from 'monaco-editor/editor/editor.api.js'
import { useAuditText } from './i18n'

const props = defineProps<{
  original: string
  modified: string
  originalLabel: string
  modifiedLabel: string
  language?: 'json' | 'plaintext'
  note?: string
  /** 内容较短（报文头）时按行数自适应高度 */
  compact?: boolean
}>()
const text = useAuditText()

const sideBySide = ref(true)
const hideUnchanged = ref(false)
const wrap = ref(false)
const changes = ref<number | null>(null)
const loadError = ref('')

const lines = computed(() => Math.max(props.original.split('\n').length, props.modified.split('\n').length))
// 固定高度：避免编辑器加载、计算差异过程中撑开 / 收缩弹窗。
const height = computed(() => (props.compact ? Math.min(420, Math.max(80, lines.value * 19 + 12)) : 560))

const host = ref<HTMLElement>()
const editor = shallowRef<Monaco.editor.IStandaloneDiffEditor>()
let monaco: typeof Monaco | undefined
let models: Monaco.editor.ITextModel[] = []
let disposed = false

const isDark = () => document.documentElement.classList.contains('dark')
useMutationObserver(document.documentElement, () => monaco?.editor.setTheme(isDark() ? 'vs-dark' : 'vs'), {
  attributes: true,
  attributeFilter: ['class']
})

function setModels() {
  const ed = editor.value
  if (!ed || !monaco) return
  const old = models
  const lang = props.language ?? 'plaintext'
  models = [monaco.editor.createModel(props.original, lang), monaco.editor.createModel(props.modified, lang)]
  changes.value = null
  ed.setModel({ original: models[0], modified: models[1] })
  old.forEach((m) => m.dispose())
}

async function create() {
  try {
    monaco = (await import('./monaco')).monaco
  } catch (e) {
    loadError.value = (e as Error)?.message ?? String(e)
    return
  }
  if (disposed || !host.value) return
  const ed = monaco.editor.createDiffEditor(host.value, {
    theme: isDark() ? 'vs-dark' : 'vs',
    readOnly: true,
    originalEditable: false,
    domReadOnly: true,
    renderSideBySide: sideBySide.value,
    useInlineViewWhenSpaceIsLimited: false,
    // 全文对比：不忽略空白，不设计算时限，不因文件大而跳过
    ignoreTrimWhitespace: false,
    maxComputationTime: 0,
    maxFileSize: 0,
    diffAlgorithm: 'advanced',
    hideUnchangedRegions: { enabled: hideUnchanged.value },
    wordWrap: wrap.value ? 'on' : 'off',
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    automaticLayout: true,
    fontSize: 12,
    lineNumbersMinChars: 3,
    renderLineHighlight: 'none',
    contextmenu: false,
    stickyScroll: { enabled: false }
  })
  editor.value = ed
  ed.onDidUpdateDiff(() => {
    changes.value = ed.getLineChanges()?.length ?? 0
  })
  setModels()
}

onMounted(create)
onBeforeUnmount(() => {
  disposed = true
  editor.value?.dispose()
  models.forEach((m) => m.dispose())
})

watch(() => [props.original, props.modified, props.language], setModels)
watch(sideBySide, (on) => editor.value?.updateOptions({ renderSideBySide: on }))
watch(hideUnchanged, (on) => editor.value?.updateOptions({ hideUnchangedRegions: { enabled: on } }))
watch(wrap, (on) => editor.value?.updateOptions({ wordWrap: on ? 'on' : 'off' }))
</script>

<style scoped>
.ad {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  min-width: 0;
}
.ad-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.75rem;
}
.ad-label {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
}
.ad-tag {
  border-radius: 999px;
  padding: 0 0.45rem;
  line-height: 1.2rem;
  background: rgb(220 252 231);
  color: rgb(21 128 61);
}
.ad-tag--base {
  background: rgb(254 226 226);
  color: rgb(185 28 28);
}
.ad-spacer {
  flex: 1;
}
.ad-muted {
  color: rgb(107 114 128);
}
.ad-bad {
  color: rgb(185 28 28);
}
.ad-note {
  margin: 0;
  font-size: 0.75rem;
  color: rgb(107 114 128);
}
.ad-btn {
  border: 1px solid rgb(209 213 219);
  border-radius: 0.375rem;
  padding: 0 0.5rem;
  font-size: 0.75rem;
  line-height: 1.4rem;
  background: transparent;
  color: inherit;
  cursor: pointer;
}
.ad-btn--on {
  background: rgb(219 234 254);
  border-color: rgb(147 197 253);
  color: rgb(29 78 216);
}
:global(.dark) .ad-btn--on {
  background: rgb(30 58 138);
  border-color: rgb(59 130 246);
  color: rgb(219 234 254);
}
.ad-host {
  min-width: 0;
  width: 100%;
  border: 1px solid rgb(229 231 235);
  border-radius: 0.375rem;
  overflow: hidden;
}
:global(.dark) .ad-host {
  border-color: rgb(55 65 81);
}
</style>
