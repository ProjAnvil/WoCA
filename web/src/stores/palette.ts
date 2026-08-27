import { defineStore } from 'pinia'
import { ref } from 'vue'

/** Open state for the global ⌘K command palette. Commands themselves are
 *  supplied by the app shell (useAppCommands) so the registry — the single
 *  source both web shortcuts and a future native menu bar can drive — lives
 *  in one place. */
export const usePaletteStore = defineStore('palette', () => {
  const open = ref(false)

  function toggle() {
    open.value = !open.value
  }

  function close() {
    open.value = false
  }

  return { open, toggle, close }
})
