<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from 'lucide-vue-next'
import { useToastStore } from '../stores/toast'

// The single toast renderer (mounted at the app root). Auto-expires; an
// explicit labeled close button dismisses early (the whole-toast click
// target was an unlabeled, keyboard-unreachable affordance); an action
// button runs its handler instead.
const { t } = useI18n()
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
    <TransitionGroup name="rise">
      <!-- Loop var is `item` on purpose: the i18n `t` from useI18n must not
           be shadowed inside the row. -->
      <div
        v-for="item in toast.toasts"
        :key="item.id"
        class="toast"
        :class="`toast-${item.kind}`"
        role="status"
      >
      <component :is="ICONS[item.kind]" :size="15" class="toast-icon" aria-hidden="true" />
      <span class="toast-message">{{ item.message }}</span>
      <button
        v-if="item.action"
        class="toast-action"
        type="button"
        @click.stop="runAction(item.id, item.action.run)"
      >
        {{ item.action.label }}
      </button>
      <button class="toast-close" type="button" :aria-label="t('common.close')" @click="toast.dismiss(item.id)">
        <X :size="13" aria-hidden="true" />
      </button>
      </div>
    </TransitionGroup>
  </div>
</template>

<style scoped>
/* rise transition classes live in styles/motion.css; the move class covers
   reordering when an older toast expires out from under a newer one. */
.rise-move {
  transition: transform var(--dur-base) var(--ease-in-out);
}
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
/* Explicit dismiss: 24px hit target, labeled (common.close). */
.toast-close {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
}
.toast-close:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.toast-close:focus-visible {
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
