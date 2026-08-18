<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useWorkspaceStore } from '../stores/workspace'

// Draggable divider between the chat column and the workspace panel. The
// pointer is captured on the handle so the drag keeps tracking after the
// cursor leaves its bounds; the workspace store clamps the resulting width
// so the chat never drops below its minimum width.
const { t } = useI18n()
const store = useWorkspaceStore()
const shell = ref<HTMLElement | null>(null)

function onDown(e: PointerEvent) {
  const el = e.currentTarget as HTMLElement
  el.setPointerCapture(e.pointerId)
  store.dragging = true
  document.body.classList.add('ws-resizing')
}

function onMove(e: PointerEvent) {
  if (!store.dragging || !shell.value) return
  store.resizeTo(shell.value.getBoundingClientRect().right - e.clientX)
}

function onUp(e: PointerEvent) {
  if (!store.dragging) return
  store.dragging = false
  document.body.classList.remove('ws-resizing')
  const el = e.currentTarget as HTMLElement
  if (el.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId)
}

// Keyboard resizing for the separator role: left widens the workspace,
// right narrows it.
function onKey(e: KeyboardEvent) {
  const step = e.shiftKey ? 96 : 24
  if (e.key === 'ArrowLeft') store.resizeTo(store.width + step)
  else if (e.key === 'ArrowRight') store.resizeTo(store.width - step)
  else return
  e.preventDefault()
}

// Re-clamp on viewport changes so a shrinking window cannot push the chat
// below its minimum width while the panel is open.
function onWindowResize() {
  store.resizeTo(store.width)
}

onMounted(() => {
  shell.value = document.querySelector('.app-shell')
  window.addEventListener('resize', onWindowResize)
})
onUnmounted(() => {
  window.removeEventListener('resize', onWindowResize)
  document.body.classList.remove('ws-resizing')
})
</script>

<template>
  <div
    class="split-handle"
    :class="{ dragging: store.dragging }"
    role="separator"
    aria-orientation="vertical"
    :aria-label="t('sidebar.resizeHandle')"
    tabindex="0"
    @pointerdown="onDown"
    @pointermove="onMove"
    @pointerup="onUp"
    @pointercancel="onUp"
    @keydown="onKey"
  ></div>
</template>
