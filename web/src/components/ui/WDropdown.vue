<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

// Minimal ARIA menu-button dropdown for split actions. The menu teleports
// to body with fixed positioning (chat action bars sit near the viewport
// bottom, so it opens upward by default and flips only when there is no
// room above but room below). Opening is click/keyboard only — never hover.
const open = defineModel<boolean>({ default: false })

const triggerEl = ref<HTMLElement | null>(null)
const menuEl = ref<HTMLElement | null>(null)
const menuStyle = ref<Record<string, string>>({})

function place() {
  const el = triggerEl.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const menuH = menuEl.value?.offsetHeight || 160
  const roomAbove = r.top > menuH + 8
  const roomBelow = r.bottom + menuH + 8 <= window.innerHeight
  const up = roomAbove || !roomBelow
  menuStyle.value = {
    position: 'fixed',
    left: `${Math.min(r.left, window.innerWidth - 200)}px`,
    ...(up ? { bottom: `${window.innerHeight - r.top + 8}px` } : { top: `${r.bottom + 8}px` }),
  }
}

function focusFirst() {
  const first = menuEl.value?.querySelector<HTMLElement>('[role="menuitem"]')
  first?.focus()
}

function onTriggerKeydown(e: KeyboardEvent) {
  if (e.key === 'ArrowDown' && !open.value) {
    e.preventDefault()
    open.value = true
    void nextTick(focusFirst)
  }
}

function onMenuKeydown(e: KeyboardEvent) {
  const items = Array.from(menuEl.value?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])
  const idx = items.indexOf(document.activeElement as HTMLElement)
  switch (e.key) {
    case 'Escape':
      e.preventDefault()
      open.value = false
      triggerEl.value?.focus()
      return
    case 'Tab':
      open.value = false
      return
    case 'ArrowDown':
      e.preventDefault()
      items[(idx + 1 + items.length) % items.length]?.focus()
      return
    case 'ArrowUp':
      e.preventDefault()
      items[(idx - 1 + items.length) % items.length]?.focus()
      return
    case 'Home':
      e.preventDefault()
      items[0]?.focus()
      return
    case 'End':
      e.preventDefault()
      items[items.length - 1]?.focus()
  }
}

function onDocPointerdown(e: PointerEvent) {
  const t = e.target as Node
  if (menuEl.value?.contains(t) || triggerEl.value?.contains(t)) return
  open.value = false
}

function closeOnExternal() {
  open.value = false
}

watch(open, async (on) => {
  if (on) {
    await nextTick()
    place()
    focusFirst()
    document.addEventListener('pointerdown', onDocPointerdown)
    window.addEventListener('scroll', closeOnExternal, true)
    window.addEventListener('resize', closeOnExternal)
  } else {
    document.removeEventListener('pointerdown', onDocPointerdown)
    window.removeEventListener('scroll', closeOnExternal, true)
    window.removeEventListener('resize', closeOnExternal)
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerdown)
  window.removeEventListener('scroll', closeOnExternal, true)
  window.removeEventListener('resize', closeOnExternal)
})
</script>

<template>
  <span
    ref="triggerEl"
    class="w-dropdown-trigger"
    aria-haspopup="menu"
    :aria-expanded="open"
    @click="open = !open"
    @keydown="onTriggerKeydown"
  >
    <slot name="trigger" :toggle="() => (open = !open)" />
  </span>
  <Teleport to="body">
    <div
      v-if="open"
      ref="menuEl"
      class="w-dropdown-menu"
      role="menu"
      :style="menuStyle"
      @keydown="onMenuKeydown"
    >
      <slot name="menu" />
    </div>
  </Teleport>
</template>
