<script setup lang="ts">
withDefaults(
  defineProps<{
    /** Required: drives aria-label and the tooltip — icon-only buttons must be named. */
    label: string
    size?: 'sm' | 'md'
    variant?: 'ghost' | 'secondary'
    badge?: number
    disabled?: boolean
  }>(),
  { size: 'md', variant: 'ghost', badge: 0, disabled: false },
)
</script>

<template>
  <button
    class="w-icon-btn"
    :class="[`w-icon-btn--${size}`, `w-icon-btn--${variant}`]"
    type="button"
    :aria-label="label"
    :title="label"
    :disabled="disabled"
  >
    <slot />
    <span v-if="badge" class="w-icon-btn-badge" aria-hidden="true">{{ badge > 99 ? '99+' : badge }}</span>
  </button>
</template>

<style scoped>
.w-icon-btn {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
  transition:
    background var(--dur-fast) var(--ease-out),
    color var(--dur-fast) var(--ease-out);
}
.w-icon-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.w-icon-btn:not(:disabled):hover {
  background: var(--surface-hover);
  color: var(--text);
}
.w-icon-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}

.w-icon-btn--sm {
  width: 28px;
  height: 28px;
}
.w-icon-btn--md {
  width: 32px;
  height: 32px;
}
.w-icon-btn--secondary {
  border-color: var(--border);
  background: var(--surface);
  color: var(--text-2);
}

/* Touch environments get a 44px hit target without changing the visual size. */
@media (pointer: coarse) {
  .w-icon-btn::after {
    content: '';
    position: absolute;
    inset: -6px;
  }
}

.w-icon-btn-badge {
  position: absolute;
  top: -4px;
  right: -4px;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: var(--radius-full);
  background: var(--danger);
  color: var(--on-danger);
  font-size: 10px;
  line-height: 16px;
  text-align: center;
}
</style>
