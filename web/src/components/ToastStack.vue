<script setup lang="ts">
import { AlertTriangle, CheckCircle2, Info, XCircle } from 'lucide-vue-next'
import { useToastStore } from '../stores/toast'

// The single toast renderer (mounted at the app root). Auto-expires; a
// click dismisses early; an action button runs its handler instead.
const toast = useToastStore()

const ICONS = {
  error: XCircle,
  warn: AlertTriangle,
  success: CheckCircle2,
  info: Info,
} as const

function runAction(id: number, run: () => void) {
  run()
  toast.dismiss(id)
}
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
      <component :is="ICONS[t.kind]" :size="15" class="toast-icon" aria-hidden="true" />
      <span class="toast-message">{{ t.message }}</span>
      <button
        v-if="t.action"
        class="toast-action"
        type="button"
        @click.stop="runAction(t.id, t.action.run)"
      >
        {{ t.action.label }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.toast-stack {
  position: fixed;
  bottom: 18px;
  left: 50%;
  transform: translateX(-50%);
  z-index: var(--z-toast);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  max-width: min(520px, 90vw);
}
.toast {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: 9px 14px;
  border-radius: var(--radius);
  border: 1px solid var(--border);
  border-left-width: 3px;
  background: var(--panel);
  color: var(--text);
  font-size: var(--text-sm);
  box-shadow: var(--shadow-md);
  cursor: pointer;
  overflow-wrap: anywhere;
}
.toast-icon {
  flex-shrink: 0;
}
.toast-message {
  flex: 1;
}
.toast-action {
  flex-shrink: 0;
  padding: 3px 10px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-sm);
  background: var(--surface);
  color: var(--text-2);
  font-family: inherit;
  font-size: var(--text-xs);
  cursor: pointer;
}
.toast-action:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.toast-action:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.toast-error {
  border-left-color: var(--danger);
}
.toast-error .toast-icon {
  color: var(--danger-text);
}
.toast-warn {
  border-left-color: var(--warn);
}
.toast-warn .toast-icon {
  color: var(--warn-text);
}
.toast-success {
  border-left-color: var(--ok);
}
.toast-success .toast-icon {
  color: var(--ok-text);
}
.toast-info {
  border-left-color: var(--info);
}
.toast-info .toast-icon {
  color: var(--info-text);
}
</style>
