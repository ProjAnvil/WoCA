<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Sidebar from './components/Sidebar.vue'
import ChatPanel from './components/ChatPanel.vue'
import WorkspaceSidebar from './components/WorkspaceSidebar.vue'
import WorkspaceSplitHandle from './components/WorkspaceSplitHandle.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import GoalPanel from './components/GoalPanel.vue'
import MemoryPanel from './components/MemoryPanel.vue'
import InterruptModal from './components/InterruptModal.vue'
import LoginGate from './components/LoginGate.vue'
import ToastStack from './components/ToastStack.vue'
import WConfirmDialog from './components/ui/WConfirmDialog.vue'
import WCommandPalette from './components/ui/WCommandPalette.vue'
import BtwPanel from './components/BtwPanel.vue'
import { useAppCommands } from './composables/useAppCommands'
import { usePaletteStore } from './stores/palette'
import { useAuthStore } from './stores/auth'
import { useSettingsStore } from './stores/settings'
import { useServerSettingsStore } from './stores/serverSettings'
import { useWorkspaceStore } from './stores/workspace'
import { useSessionStore } from './stores/sessions'
import { useMemoryStore } from './stores/memory'
import { useCompletionsStore } from './stores/completions'

// Initializing the settings store applies the persisted/system theme.
const settings = useSettingsStore()
const serverSettings = useServerSettingsStore()
const workspace = useWorkspaceStore()
const sessions = useSessionStore()
const memory = useMemoryStore()
const completions = useCompletionsStore()
const auth = useAuthStore()
const palette = usePaletteStore()
const commands = useAppCommands()

/* ---------------------- global shortcuts + command palette ---------------- */

// ⌘K toggles the palette (the everything-else fallback), ⌘J toggles the
// workspace panel. Double-Esc (<300ms) interrupts the active run — but Esc
// inside the composer belongs to autocomplete close, and while the palette
// is open it belongs to the palette, so both contexts are excluded.
let lastEsc = 0

function onGlobalKeydown(e: KeyboardEvent) {
  const meta = e.metaKey || e.ctrlKey
  if (meta && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    palette.toggle()
    return
  }
  if (meta && e.key.toLowerCase() === 'j') {
    e.preventDefault()
    workspace.togglePanel()
    return
  }
  if (e.key === 'Escape' && !e.isComposing) {
    if (palette.open) return
    if (document.activeElement instanceof HTMLTextAreaElement) return
    const now = Date.now()
    if (now - lastEsc < 300 && ['running', 'awaiting', 'queued'].includes(sessions.activeStatus)) {
      e.preventDefault()
      void sessions.cancel()
    }
    lastEsc = now
  }
}

onMounted(() => window.addEventListener('keydown', onGlobalKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onGlobalKeydown))

// Theme cross-fade: the @property-registered color tokens (tokens.css) morph
// 240ms on theme toggle. Opt in only after first paint so the boot-time
// dark-class application never animates.
onMounted(() => document.documentElement.classList.add('theme-anim'))

// Gate → app handoff: when the login gate closes after a successful login,
// the shell underneath rises once (motion.css .app-entering, 400ms) and
// overlaps the gate's 320ms dissolve into one continuous cross-fade.
const appEntering = ref(false)
let appEnterTimer = 0
watch(
  () => auth.gateOpen,
  (open, was) => {
    if (was && !open) {
      appEntering.value = true
      window.clearTimeout(appEnterTimer)
      appEnterTimer = window.setTimeout(() => (appEntering.value = false), 560)
    }
  },
)

onMounted(() => {
  void auth.probe()
  void sessions.init()
  void workspace.load()
  // Server-backed settings must load at startup too: chat features gated on
  // them (the bubble debug-copy button needs debug_log) would otherwise stay
  // hidden until the settings panel is opened — a reload regressed the debug
  // button until the user re-toggled the switch.
  void serverSettings.loadServer()
})

// The file sidebar and the "/" "@" autocomplete follow the active session's
// workspace: switching to a session in another project reloads them from
// that directory instead of the server's startup one.
watch(
  () => sessions.activeWorkspace.path,
  (path, prev) => {
    if (path !== prev) {
      void workspace.load(path || undefined)
      void completions.load(path ?? '')
    }
  },
)
</script>

<template>
  <div class="app-shell" :class="{ 'app-entering': appEntering }">
    <Sidebar />
    <main class="main">
      <ChatPanel />
    </main>
    <WorkspaceSplitHandle v-if="workspace.panelOpen" />
    <WorkspaceSidebar />
    <Transition name="panel-pop">
      <SettingsPanel v-if="settings.panelOpen" />
    </Transition>
    <Transition name="panel-pop">
      <GoalPanel v-if="sessions.goalPanelOpen" />
    </Transition>
    <Transition name="panel-pop">
      <MemoryPanel v-if="memory.panelOpen" />
    </Transition>
    <InterruptModal v-if="sessions.activeInterrupt" />
    <Transition name="gate-dissolve">
      <LoginGate v-if="auth.gateOpen" :revoked="auth.gateCode === 'revoked'" @authenticated="auth.onAuthenticated" />
    </Transition>
    <WConfirmDialog />
    <BtwPanel />
    <WCommandPalette :commands="commands" />
    <ToastStack />
  </div>
</template>
