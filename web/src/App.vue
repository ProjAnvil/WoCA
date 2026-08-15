<script setup lang="ts">
import { onMounted } from 'vue'
import Sidebar from './components/Sidebar.vue'
import ChatPanel from './components/ChatPanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import MarkdownView from './components/MarkdownView.vue'
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
    <SettingsPanel v-if="settings.panelOpen" />
    <MarkdownView v-if="workspace.preview" />
    <InterruptModal v-if="sessions.activeInterrupt" />
  </div>
</template>
