<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { X } from 'lucide-vue-next'
import { dismissToast, useToasts } from '../composables/useToast'

const { t } = useI18n()
const toasts = useToasts()
</script>

<template>
  <div class="toast toast-top toast-end z-50 ops-toast-stack" aria-live="polite">
    <div
      v-for="item in toasts"
      :key="item.id"
      class="alert ops-toast"
      :class="item.kind === 'error' ? 'ops-toast-error' : 'ops-toast-neutral'"
      role="status"
    >
      <span class="ops-toast-text">{{ item.message }}</span>
      <button class="ops-toast-x" :aria-label="t('common.close')" @click="dismissToast(item.id)">
        <X :size="13" />
      </button>
    </div>
  </div>
</template>

<style scoped>
.ops-toast-stack {
  font-size: 12.5px;
}
.ops-toast {
  background: #fff;
  border: 1px solid #cbd2d9;
  border-radius: 8px;
  box-shadow: 0 4px 14px rgba(31, 41, 51, 0.12);
  color: #1f2933;
  padding: 8px 10px;
  gap: 10px;
  max-width: 420px;
}
.ops-toast-error {
  border-color: #c0392b;
}
.ops-toast-error .ops-toast-text {
  color: #c0392b;
}
.ops-toast-x {
  background: none;
  border: 0;
  cursor: pointer;
  padding: 2px;
  color: #616e7c;
  display: inline-flex;
}
.ops-toast-x:hover {
  color: #1f2933;
}
</style>
