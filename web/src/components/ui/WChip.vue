<script setup lang="ts">
import type { Component } from 'vue'

withDefaults(
  defineProps<{
    selected?: boolean
    tone?: 'neutral' | 'brand' | 'ok' | 'warn' | 'danger'
    icon?: Component
    disabled?: boolean
  }>(),
  { selected: false, tone: 'neutral', disabled: false },
)
</script>

<template>
  <button
    class="w-chip"
    :class="[`w-chip--${tone}`, { 'w-chip--selected': selected }]"
    type="button"
    :aria-pressed="selected"
    :disabled="disabled"
  >
    <component :is="icon" v-if="icon" :size="13" />
    <slot />
  </button>
</template>

<style scoped>
.w-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 26px;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-full);
  background: var(--surface);
  color: var(--text-2);
  font-family: inherit;
  font-size: var(--text-sm);
  white-space: nowrap;
  cursor: pointer;
  transition:
    background var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out),
    color var(--dur-fast) var(--ease-out);
}
.w-chip:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.w-chip:not(:disabled):hover {
  background: var(--surface-hover);
}
.w-chip:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}

.w-chip--selected {
  border-color: color-mix(in srgb, var(--brand) 55%, var(--border));
  background: var(--brand-soft);
  color: var(--brand-text);
}

.w-chip--ok.w-chip--selected {
  border-color: color-mix(in srgb, var(--ok) 55%, var(--border));
  background: var(--ok-soft);
  color: var(--ok-text);
}
.w-chip--warn.w-chip--selected {
  border-color: color-mix(in srgb, var(--warn) 55%, var(--border));
  background: var(--warn-soft);
  color: var(--warn-text);
}
.w-chip--danger.w-chip--selected {
  border-color: color-mix(in srgb, var(--danger) 55%, var(--border));
  background: var(--danger-soft);
  color: var(--danger-text);
}
</style>
