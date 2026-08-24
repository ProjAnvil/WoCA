<script setup lang="ts">
import { onMounted, watch } from 'vue'
import Sidebar from './components/Sidebar.vue'
import ChatPanel from './components/ChatPanel.vue'
import WorkspaceSidebar from './components/WorkspaceSidebar.vue'
import WorkspaceSplitHandle from './components/WorkspaceSplitHandle.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import GoalPanel from './components/GoalPanel.vue'
import MemoryPanel from './components/MemoryPanel.vue'
import InterruptModal from './components/InterruptModal.vue'
import LoginGate from './components/LoginGate.vue'
import { useAuthStore } from './stores/auth'
import { useSettingsStore } from './stores/settings'
import { useWorkspaceStore } from './stores/workspace'
import { useSessionStore } from './stores/sessions'
import { useMemoryStore } from './stores/memory'
import { useCompletionsStore } from './stores/completions'

// Initializing the settings store applies the persisted/system theme.
const settings = useSettingsStore()
const workspace = useWorkspaceStore()
const sessions = useSessionStore()
const memory = useMemoryStore()
const completions = useCompletionsStore()
const auth = useAuthStore()

onMounted(() => {
  void auth.probe()
  void sessions.init()
  void workspace.load()
  // Server-backed settings must load at startup too: chat features gated on
  // them (the bubble debug-copy button needs debug_log) would otherwise stay
  // hidden until the settings panel is opened — a reload regressed the debug
  // button until the user re-toggled the switch.
  void settings.loadServer()
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
  <div class="app-shell">
    <Sidebar />
    <main class="main">
      <ChatPanel />
    </main>
    <WorkspaceSplitHandle v-if="workspace.panelOpen" />
    <WorkspaceSidebar />
    <SettingsPanel v-if="settings.panelOpen" />
    <GoalPanel v-if="sessions.goalPanelOpen" />
    <MemoryPanel v-if="memory.panelOpen" />
    <InterruptModal v-if="sessions.activeInterrupt" />
    <LoginGate v-if="auth.gateOpen" :revoked="auth.gateCode === 'revoked'" @authenticated="auth.onAuthenticated" />
  </div>
</template>
