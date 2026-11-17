import { defineStore } from 'pinia'
import { ref } from 'vue'

// Tiny shell-chrome slice: state shared across components that has no domain
// of its own. Currently only the ≤640px session-sidebar drawer — the
// hamburger lives in ChatPanel's header, the drawer + backdrop in Sidebar.
// Desktop shells ignore it: the drawer styles are media-query scoped.
export const useUiStore = defineStore('ui', () => {
  /** Whether the left session sidebar drawer is open (≤640px shell). */
  const sidebarOpen = ref(false)

  function openSidebar() {
    sidebarOpen.value = true
  }

  function closeSidebar() {
    sidebarOpen.value = false
  }

  function toggleSidebar() {
    sidebarOpen.value = !sidebarOpen.value
  }

  return { sidebarOpen, openSidebar, closeSidebar, toggleSidebar }
})
