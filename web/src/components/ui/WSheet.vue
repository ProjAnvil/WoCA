<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { focusFirst, useFocusTrap } from '../../composables/useFocusTrap'

// Right-side slide-in sheet for context tools the user keeps open while
// reading the chat (settings / goal / memory). Same layer contract as
// WModal: Teleport, focus trap, Esc closes, background scroll locked.
// Narrow screens turn it into a bottom drawer.
withDefaults(
  defineProps<{
    title?: string
    size?: 'sm' | 'md' | 'lg'
  }>(),
  { title: '', size: 'md' },
)
const emit = defineEmits<{ close: [] }>()

const panel = ref<HTMLElement | null>(null)
const active = computed(() => true) // mounted === open (parent uses v-if)
useFocusTrap(panel, active, { esc: () => emit('close') })

// Mounted-only component: the trap's immediate watcher runs pre-mount, so
// the initial focus lands here explicitly once the sheet is in the DOM.
onMounted(async () => {
  await Promise.resolve()
  if (panel.value) focusFirst(panel.value)
})
</script>

<template>
  <Teleport to="body">
    <div class="w-sheet-backdrop" @click.self="emit('close')">
      <aside
        ref="panel"
        class="w-sheet"
        :class="`w-sheet--${size}`"
        role="dialog"
        aria-modal="true"
        :aria-label="title || undefined"
        tabindex="-1"
      >
        <slot />
      </aside>
    </div>
  </Teleport>
</template>

<style scoped>
.w-sheet-backdrop {
  position: fixed;
  inset: 0;
  z-index: var(--z-sheet);
  background: var(--scrim);
  display: flex;
  justify-content: flex-end;
}
.w-sheet {
  display: flex;
  flex-direction: column;
  width: 100%;
  height: 100%;
  background: var(--surface);
  border-left: 1px solid var(--border);
  box-shadow: var(--shadow-overlay);
  outline: none;
  overflow: hidden;
  animation: w-sheet-in var(--dur-slow) var(--ease-out);
}
.w-sheet--sm {
  max-width: 480px;
}
.w-sheet--md {
  max-width: 640px;
}
.w-sheet--lg {
  max-width: 800px;
}

@keyframes w-sheet-in {
  from {
    transform: translateX(24px);
    opacity: 0.6;
  }
  to {
    transform: translateX(0);
    opacity: 1;
  }
}

@media (prefers-reduced-motion: reduce) {
  .w-sheet {
    animation: none;
  }
}

/* Narrow viewports: bottom drawer instead of a side sheet. */
@media (max-width: 768px) {
  .w-sheet-backdrop {
    align-items: flex-end;
  }
  .w-sheet {
    max-width: none;
    width: 100%;
    height: auto;
    max-height: 85vh;
    border-left: none;
    border-top: 1px solid var(--border);
    animation-name: w-sheet-in-vertical;
  }
  @keyframes w-sheet-in-vertical {
    from {
      transform: translateY(24px);
      opacity: 0.6;
    }
    to {
      transform: translateY(0);
      opacity: 1;
    }
  }
}
</style>
