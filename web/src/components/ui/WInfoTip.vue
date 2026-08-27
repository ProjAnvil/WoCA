<script setup lang="ts">
// Inline "ⓘ" affordance for secondary explanations: hover (or keyboard
// focus) reveals the text via a native tooltip. Primary descriptions stay
// in the UI; this replaces the small always-on hint lines that crowded
// settings rows.
withDefaults(defineProps<{ text: string }>(), { text: '' })
</script>

<template>
  <span class="w-infotip" tabindex="0" :title="text" :aria-label="text" role="note">
    <svg viewBox="0 0 24 24" width="13" height="13" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <circle cx="12" cy="12" r="10" />
      <path d="M12 16v-4" />
      <path d="M12 8h.01" />
    </svg>
  </span>
</template>

<style scoped>
.w-infotip {
  display: inline-flex;
  align-items: center;
  margin-left: 5px;
  color: var(--faint);
  border-radius: var(--radius-xs);
  cursor: help;
  vertical-align: middle;
}
.w-infotip:hover,
.w-infotip:focus-visible {
  color: var(--text-2);
}
.w-infotip:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
</style>
