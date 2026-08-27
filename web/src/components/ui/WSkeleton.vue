<script setup lang="ts">
// Loading skeleton shapes: the >1s-wait replacement for bare spinners and
// plain "loading…" text. Purely decorative (aria-hidden) — callers wrap in
// role="status" + a .sr-only announcement.
withDefaults(
  defineProps<{
    variant?: 'text' | 'circle' | 'rect' | 'paragraph'
    /** CSS width for every shape ('120px', '62%', …). */
    width?: string
    /** CSS height; defaults per variant (text 12px, rect 48px). */
    height?: string
    /** Number of shapes (paragraph tapers its final line). */
    repeat?: number
  }>(),
  { variant: 'text', width: '', height: '', repeat: 1 },
)
</script>

<template>
  <div class="w-skeleton" :class="{ 'w-skeleton-paragraph': variant === 'paragraph' }" aria-hidden="true">
    <span
      v-for="i in repeat"
      :key="i"
      class="w-skeleton-shape"
      :class="variant === 'paragraph' ? 'w-skeleton-text' : `w-skeleton-${variant}`"
      :style="{ width: width || undefined, height: height || undefined }"
    />
  </div>
</template>

<style scoped>
.w-skeleton {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.w-skeleton-shape {
  display: block;
  background: var(--surface-active);
  animation: w-skeleton-pulse 1.2s ease-in-out infinite;
}
@keyframes w-skeleton-pulse {
  0%,
  100% {
    opacity: 0.55;
  }
  50% {
    opacity: 1;
  }
}
.w-skeleton-text {
  height: 12px;
  border-radius: var(--radius-xs);
}
.w-skeleton-rect {
  height: 48px;
  border-radius: var(--radius);
}
.w-skeleton-circle {
  border-radius: var(--radius-full);
}
/* Paragraphs read as text blocks: the final line tapers off. */
.w-skeleton-paragraph .w-skeleton-shape:last-child {
  width: 62%;
}
</style>
