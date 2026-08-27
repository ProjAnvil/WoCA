<script setup lang="ts">
import { ChevronRight } from 'lucide-vue-next'

// Unified collapsible card shell for everything embedded in the message
// stream (tool calls, reasoning, goal narration): tone rail + icon + title
// + meta + live breathing dot on the header, animated height below.
// Always controlled — callers own the open state (v-model:open).
withDefaults(
  defineProps<{
    title: string
    meta?: string
    tone?: 'neutral' | 'brand'
    live?: boolean
    open: boolean
  }>(),
  { meta: '', tone: 'neutral', live: false },
)
const emit = defineEmits<{ 'update:open': [boolean] }>()
</script>

<template>
  <div class="w-collapse-card" :class="[`w-collapse-card--${tone}`, { live }]">
    <button class="w-collapse-head" type="button" :aria-expanded="open" @click="emit('update:open', !open)">
      <span class="w-collapse-rail" aria-hidden="true" />
      <slot name="icon" />
      <span class="w-collapse-title">{{ title }}</span>
      <span v-if="live" class="w-collapse-live" aria-hidden="true" />
      <span v-if="meta" class="w-collapse-meta">{{ meta }}</span>
      <ChevronRight :size="13" class="w-collapse-chevron" :class="{ open }" aria-hidden="true" />
    </button>
    <div class="w-collapse-body" :class="{ open }">
      <div class="w-collapse-body-inner" :aria-hidden="open ? undefined : 'true'" :inert="!open">
        <slot />
      </div>
    </div>
  </div>
</template>

<style scoped>
.w-collapse-card {
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--bg-soft);
  overflow: hidden;
}
.w-collapse-card.w-collapse-card--brand {
  border-color: color-mix(in srgb, var(--brand) 40%, var(--border));
}
.w-collapse-card.live {
  border-color: color-mix(in srgb, var(--brand) 55%, var(--border));
}

.w-collapse-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  width: 100%;
  min-width: 0;
  padding: var(--space-3) var(--space-4);
  border: none;
  background: transparent;
  color: var(--text-2);
  font: inherit;
  font-size: var(--text-sm);
  font-weight: 500;
  text-align: left;
  cursor: pointer;
}
.w-collapse-head:hover {
  background: color-mix(in srgb, var(--surface-hover) 55%, transparent);
  color: var(--text);
}
.w-collapse-head:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: -2px;
}

.w-collapse-rail {
  align-self: stretch;
  width: 3px;
  margin: calc(-1 * var(--space-3)) var(--space-1) calc(-1 * var(--space-3)) calc(-1 * var(--space-4));
  background: var(--border-strong);
}
.w-collapse-card--brand .w-collapse-rail {
  background: var(--brand);
}

.w-collapse-title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.w-collapse-live {
  width: 7px;
  height: 7px;
  flex-shrink: 0;
  border-radius: var(--radius-full);
  background: var(--brand);
  animation: w-collapse-breathe 1.6s ease-in-out infinite;
}
@keyframes w-collapse-breathe {
  0%,
  100% {
    opacity: 0.4;
  }
  50% {
    opacity: 1;
  }
}

.w-collapse-meta {
  margin-left: auto;
  flex-shrink: 0;
  color: var(--faint);
  font-size: var(--text-xs);
}

.w-collapse-chevron {
  flex-shrink: 0;
  color: var(--muted);
  transition: transform var(--dur-base) var(--ease-out);
}
.w-collapse-chevron.open {
  transform: rotate(90deg);
}

.w-collapse-body {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows var(--dur-slow) var(--ease-out);
}
.w-collapse-body.open {
  grid-template-rows: 1fr;
}
.w-collapse-body-inner {
  min-height: 0;
  overflow: hidden;
}
</style>
