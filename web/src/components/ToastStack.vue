<script setup lang="ts">
import { useToastStore } from '../stores/toast'

// The single toast renderer (mounted at the app root). Errors auto-expire;
// a click dismisses early.
const toast = useToastStore()
</script>

<template>
  <div v-if="toast.toasts.length" class="toast-stack" aria-live="polite">
    <div
      v-for="t in toast.toasts"
      :key="t.id"
      class="toast"
      :class="`toast-${t.kind}`"
      role="status"
      @click="toast.dismiss(t.id)"
    >
      {{ t.message }}
    </div>
  </div>
</template>

<style scoped>
.toast-stack {
  position: fixed;
  bottom: 18px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 400;
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-width: min(520px, 90vw);
}
.toast {
  padding: 9px 14px;
  border-radius: var(--radius, 8px);
  border: 1px solid var(--border);
  background: var(--panel, #1b1e24);
  color: var(--text, #e6e8ec);
  font-size: 13px;
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.35);
  cursor: pointer;
  overflow-wrap: anywhere;
}
.toast-error {
  border-color: var(--danger, #e5484d);
}
.toast-info {
  border-color: var(--focus);
}
</style>
