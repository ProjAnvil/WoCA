<script setup lang="ts">
import { onMounted } from 'vue'
import Sidebar from './components/Sidebar.vue'
import ChatPanel from './components/ChatPanel.vue'
import WorkspaceSidebar from './components/WorkspaceSidebar.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import GoalPanel from './components/GoalPanel.vue'
import InterruptModal from './components/InterruptModal.vue'
import { useSettingsStore } from './stores/settings'
import { useWorkspaceStore } from './stores/workspace'
import { useSessionStore } from './stores/sessions'

// Initializing the settings store applies the persisted/system theme.
const settings = useSettingsStore()
const workspace = useWorkspaceStore()
const sessions = useSessionStore()

onMounted(() => {
  void sessions.init()
  void workspace.load()
})
</script>

<template>
  <div class="app-shell">
    <Sidebar />
    <main class="main">
      <ChatPanel />
    </main>
    <WorkspaceSidebar />
    <SettingsPanel v-if="settings.panelOpen" />
    <GoalPanel v-if="sessions.goalPanelOpen" />
    <InterruptModal v-if="sessions.activeInterrupt" />
  </div>
</template>
