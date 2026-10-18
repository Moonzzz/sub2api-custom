<template>
  <span class="audit-entry">
    <button
      type="button"
      class="audit-entry__btn"
      :title="text.openTitle"
      :data-usage-id="usageId"
      data-testid="audit-archive-open"
      :disabled="!usageId"
      @click.stop="open = true"
    >
      {{ text.open }}
    </button>
    <AuditArchiveModal v-if="open && usageId" :key="usageId" :usage-id="usageId" @close="open = false" />
  </span>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AuditArchiveModal from './AuditArchiveModal.vue'
import { useAuditText } from './i18n'

const props = defineProps<{ row: { id?: number } | null | undefined }>()
const text = useAuditText()
const open = ref(false)
// 入口绑定行对象本身的 id：表格复用 DOM（分页/排序）时行变化会立即关闭弹窗，避免串日志。
const usageId = computed(() => (typeof props.row?.id === 'number' ? props.row.id : 0))
watch(usageId, () => {
  open.value = false
})
</script>

<style scoped>
.audit-entry {
  display: inline-flex;
  margin-left: 0.5rem;
  vertical-align: middle;
}
.audit-entry__btn {
  border: 1px solid rgb(209 213 219);
  border-radius: 0.375rem;
  padding: 0 0.4rem;
  font-size: 0.7rem;
  line-height: 1.25rem;
  color: rgb(37 99 235);
  background: transparent;
  cursor: pointer;
  white-space: nowrap;
}
.audit-entry__btn:hover {
  background: rgb(239 246 255);
}
.audit-entry__btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}
:global(.dark) .audit-entry__btn {
  border-color: rgb(75 85 99);
  color: rgb(96 165 250);
}
:global(.dark) .audit-entry__btn:hover {
  background: rgb(30 41 59);
}
</style>
