<script setup lang="ts">
import { computed, ref } from 'vue'
import { useFocusTrap } from '../../composables/useFocusTrap'

const props = withDefaults(
  defineProps<{
    open: boolean
    title?: string
    size?: 'md' | 'lg'
    /** Persistent modals ignore Esc and backdrop clicks (gates, forced steps). */
    persistent?: boolean
  }>(),
  { title: '', size: 'md', persistent: false },
)
const emit = defineEmits<{ close: [] }>()

const panel = ref<HTMLElement | null>(null)
const active = computed(() => props.open)
useFocusTrap(panel, active, {
  esc: () => {
    if (!props.persistent) emit('close')
  },
})
</script>

<template>
  <Teleport to="body">
    <Transition name="w-modal">
      <div v-if="open" class="w-modal-backdrop" @click.self="!persistent && emit('close')">
        <div
          ref="panel"
          class="w-modal-panel"
          :class="`w-modal-panel--${size}`"
          role="dialog"
          aria-modal="true"
          :aria-label="title || undefined"
          tabindex="-1"
        >
          <slot />
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.w-modal-backdrop {
  position: fixed;
  inset: 0;
  z-index: var(--z-modal);
  display: grid;
  place-items: center;
  padding: var(--space-5);
  background: var(--scrim);
}
.w-modal-panel {
  width: 100%;
  max-height: calc(100vh - 2 * var(--space-8));
  overflow: auto;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-overlay);
  outline: none;
}
.w-modal-panel--md {
  max-width: 480px;
}
.w-modal-panel--lg {
  max-width: 640px;
}

.w-modal-enter-active,
.w-modal-leave-active {
  transition: opacity var(--dur-slow) var(--ease-out);
}
.w-modal-enter-active .w-modal-panel {
  transition: transform var(--dur-slow) var(--ease-out);
}
.w-modal-enter-from,
.w-modal-leave-to {
  opacity: 0;
}
.w-modal-enter-from .w-modal-panel {
  transform: scale(0.98);
}
</style>
