<script setup lang="ts">
// Inline SVG avatars for the chat bubbles: woca is a rounded-square bot mark
// echoing the brand; the user is a circular person glyph.
withDefaults(defineProps<{ kind: 'user' | 'woca'; size?: number }>(), { size: 28 })
</script>

<template>
  <span
    class="avatar"
    :class="kind"
    :style="{ width: `${size}px`, height: `${size}px` }"
    aria-hidden="true"
  >
    <svg
      v-if="kind === 'woca'"
      :width="size ? size * 0.62 : 17"
      :height="size ? size * 0.62 : 17"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.9"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <!-- bot antenna -->
      <circle cx="12" cy="4.6" r="1.5" fill="currentColor" stroke="none" />
      <path d="M12 6.1v2.2" />
      <!-- head -->
      <rect x="4" y="8.3" width="16" height="11.2" rx="3.4" />
      <!-- eyes -->
      <circle cx="9" cy="13.4" r="1.15" fill="currentColor" stroke="none" />
      <circle cx="15" cy="13.4" r="1.15" fill="currentColor" stroke="none" />
      <!-- mouth -->
      <path d="M9.6 16.7h4.8" />
    </svg>
    <svg
      v-else
      :width="size ? size * 0.6 : 17"
      :height="size ? size * 0.6 : 17"
      viewBox="0 0 24 24"
      fill="currentColor"
    >
      <circle cx="12" cy="8.6" r="3.5" />
      <path d="M12 13.6c-3.9 0-6.8 2.3-6.8 5.6 0 .7.5 1.2 1.2 1.2h11.2c.7 0 1.2-.5 1.2-1.2 0-3.3-2.9-5.6-6.8-5.6Z" />
    </svg>
  </span>
</template>
