<script setup lang="ts">
import type { Component } from 'vue'
import { Loader2 } from 'lucide-vue-next'

withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md' | 'lg'
    loading?: boolean
    icon?: Component
    block?: boolean
    disabled?: boolean
    type?: 'button' | 'submit'
  }>(),
  {
    variant: 'secondary',
    size: 'md',
    loading: false,
    block: false,
    disabled: false,
    type: 'button',
  },
)
</script>

<template>
  <button
    class="w-btn"
    :class="[`w-btn--${variant}`, `w-btn--${size}`, { 'w-btn--block': block }]"
    :type="type"
    :disabled="disabled || loading"
    :aria-busy="loading || undefined"
  >
    <Loader2 v-if="loading" :size="size === 'sm' ? 13 : 15" class="w-btn-spinner" />
    <component :is="icon" v-else-if="icon" :size="size === 'sm' ? 13 : 15" />
    <slot />
  </button>
</template>

<style scoped>
.w-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  font-family: inherit;
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  transition:
    background var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out),
    color var(--dur-fast) var(--ease-out),
    opacity var(--dur-fast) var(--ease-out);
}
.w-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.w-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}

.w-btn--sm {
  height: 28px;
  padding: 0 12px;
  font-size: var(--text-sm);
}
.w-btn--md {
  height: 32px;
  padding: 0 14px;
  font-size: var(--text-base);
}
.w-btn--lg {
  height: 40px;
  padding: 0 18px;
  font-size: var(--text-base);
}
.w-btn--block {
  width: 100%;
}

.w-btn--primary {
  background: var(--accent);
  color: var(--on-accent);
}
.w-btn--primary:not(:disabled):hover {
  background: var(--accent-hover);
}
.w-btn--primary:not(:disabled):active {
  transform: scale(0.98);
}

.w-btn--secondary {
  background: var(--surface);
  border-color: var(--border);
  color: var(--text);
}
.w-btn--secondary:not(:disabled):hover {
  background: var(--surface-hover);
}

.w-btn--ghost {
  background: transparent;
  color: var(--text-2);
}
.w-btn--ghost:not(:disabled):hover {
  background: var(--surface-hover);
  color: var(--text);
}

.w-btn--danger {
  background: var(--danger);
  color: var(--on-danger);
}
.w-btn--danger:not(:disabled):hover {
  background: color-mix(in srgb, var(--danger) 85%, var(--surface));
}

.w-btn-spinner {
  animation: w-btn-spin 0.9s linear infinite;
}
@keyframes w-btn-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
