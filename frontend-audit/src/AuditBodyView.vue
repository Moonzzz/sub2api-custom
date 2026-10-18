<template>
  <div class="ab" data-testid="audit-body">
    <div class="ab-bar">
      <span v-if="loading" class="ab-muted">{{ text.loading }}</span>
      <span v-else-if="loadError" class="ab-note--bad">{{ loadError }}</span>
      <span class="ab-spacer"></span>
      <template v-if="json">
        <button type="button" class="ab-btn" :disabled="!ready" data-testid="audit-body-unfold" @click="run('editor.unfoldAll')">{{ text.expandAll }}</button>
        <button type="button" class="ab-btn" :disabled="!ready" data-testid="audit-body-fold" @click="run('editor.foldAll')">{{ text.collapseAll }}</button>
      </template>
      <button
        type="button"
        class="ab-btn"
        :class="{ 'ab-btn--on': wrap }"
        :aria-pressed="wrap"
        data-testid="audit-body-wrap"
        @click="wrap = !wrap"
      >{{ text.wrapLines }}</button>
      <button type="button" class="ab-btn" data-testid="audit-body-copy" @click="copyRaw">{{ text.copy }}</button>
    </div>
    <p v-if="copyMsg" class="ab-note" :class="copyOk ? 'ab-note--ok' : 'ab-note--bad'" data-testid="audit-body-copy-result">{{ copyMsg }}</p>
    <!-- 原始文本直接交给 Monaco：JSON 排版、高亮、折叠、换行均由编辑器完成 -->
    <div ref="host" class="ab-host" :style="{ height: `${height}px` }"></div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useClipboard, useMutationObserver } from '@vueuse/core'
import type * as Monaco from 'monaco-editor/editor/editor.api.js'
import { useAuditText } from './i18n'

const props = defineProps<{
  text: string
  /** 正文已按展示上限截断：复制时提示下载完整内容 */
  truncated?: boolean
}>()
const text = useAuditText()

const MAX_HEIGHT = 448
const MIN_HEIGHT = 60

// 只用于选择语言模式（不解析）：以 { / [ 开头按 JSON 展示
const json = computed(() => /^\s*[[{]/.test(props.text))
const wrap = ref(false)
// 先按文本估算高度（单行长 JSON 排版后通常超过上限），编辑器加载与排版过程中不再明显撑高。
function estimateHeight(t: string): number {
  const lines = t.split('\n').length
  if (lines === 1 && /^\s*[[{]/.test(t) && t.length > 200) return MAX_HEIGHT
  return Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, lines * 18 + 10))
}
const height = ref(estimateHeight(props.text))
const loading = ref(true)
const loadError = ref('')
const ready = ref(false)

const host = ref<HTMLElement>()
const editor = shallowRef<Monaco.editor.IStandaloneCodeEditor>()
let monaco: typeof Monaco | undefined
let disposed = false

const isDark = () => document.documentElement.classList.contains('dark')
useMutationObserver(document.documentElement, () => monaco?.editor.setTheme(isDark() ? 'vs-dark' : 'vs'), {
  attributes: true,
  attributeFilter: ['class']
})

/** 压缩成一行的 JSON 用 Monaco 自带格式化排版（按文本处理，数字原样保留）；JSON 语言服务在 worker 中就绪前重试。 */
async function format(ed: Monaco.editor.IStandaloneCodeEditor) {
  const model = ed.getModel()
  if (!json.value || !model || model.getLineCount() > 1) return
  for (let i = 0; i < 30 && !disposed && model.getLineCount() === 1; i++) {
    ed.updateOptions({ readOnly: false })
    await ed.getAction('editor.action.formatDocument')?.run()
    ed.updateOptions({ readOnly: true })
    if (model.getLineCount() === 1) await new Promise((r) => setTimeout(r, 100))
  }
}

async function create() {
  try {
    monaco = (await import('./monaco')).monaco
  } catch (e) {
    loading.value = false
    loadError.value = (e as Error)?.message ?? String(e)
    return
  }
  if (disposed || !host.value) return
  const ed = monaco.editor.create(host.value, {
    value: props.text || text.value.empty,
    language: json.value ? 'json' : 'plaintext',
    theme: isDark() ? 'vs-dark' : 'vs',
    readOnly: true,
    domReadOnly: true,
    wordWrap: wrap.value ? 'on' : 'off',
    folding: true,
    showFoldingControls: 'always',
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
  ed.onDidContentSizeChange(() => {
    height.value = Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, ed.getContentHeight()))
  })
  loading.value = false
  await format(ed)
  ready.value = true
}

function run(action: string) {
  editor.value?.getAction(action)?.run()
}

onMounted(create)
onBeforeUnmount(() => {
  disposed = true
  editor.value?.getModel()?.dispose()
  editor.value?.dispose()
})

watch(
  () => props.text,
  async (t) => {
    const ed = editor.value
    const model = ed?.getModel()
    if (!ed || !model || !monaco) return
    model.setValue(t || text.value.empty)
    monaco.editor.setModelLanguage(model, json.value ? 'json' : 'plaintext')
    await format(ed)
  }
)
watch(wrap, (on) => editor.value?.updateOptions({ wordWrap: on ? 'on' : 'off' }))

// 复制原文（与归档一致，不受排版 / 折叠影响）。
const { copy } = useClipboard({ legacy: true })
const copyMsg = ref('')
const copyOk = ref(true)
let copyTimer: ReturnType<typeof setTimeout> | undefined
async function copyRaw() {
  try {
    await copy(props.text)
    copyOk.value = true
    copyMsg.value = props.truncated ? text.value.copiedTruncated : text.value.copied
  } catch {
    copyOk.value = false
    copyMsg.value = text.value.copyFailed
  }
  clearTimeout(copyTimer)
  copyTimer = setTimeout(() => (copyMsg.value = ''), 3000)
}
</script>

<style scoped>
.ab {
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  min-width: 0;
}
.ab-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.4rem;
}
.ab-spacer {
  flex: 1;
}
.ab-muted {
  color: rgb(107 114 128);
  font-size: 0.75rem;
}
.ab-btn {
  border: 1px solid rgb(209 213 219);
  border-radius: 0.375rem;
  padding: 0 0.5rem;
  font-size: 0.75rem;
  line-height: 1.4rem;
  background: transparent;
  color: inherit;
  cursor: pointer;
}
.ab-btn:disabled {
  opacity: 0.5;
}
.ab-btn--on {
  background: rgb(219 234 254);
  border-color: rgb(147 197 253);
  color: rgb(29 78 216);
}
:global(.dark) .ab-btn--on {
  background: rgb(30 58 138);
  border-color: rgb(59 130 246);
  color: rgb(219 234 254);
}
.ab-note {
  margin: 0;
  font-size: 0.75rem;
}
.ab-note--ok {
  color: rgb(21 128 61);
}
.ab-note--bad {
  color: rgb(185 28 28);
}
.ab-host {
  min-width: 0;
  width: 100%;
  border: 1px solid rgb(229 231 235);
  border-radius: 0.375rem;
  overflow: hidden;
}
:global(.dark) .ab-host {
  border-color: rgb(55 65 81);
}
</style>
