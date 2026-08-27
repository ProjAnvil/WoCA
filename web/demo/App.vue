<script setup lang="ts">
import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import Sidebar from '@/components/Sidebar.vue'
import ChatPanel from '@/components/ChatPanel.vue'
import WorkspaceSidebar from '@/components/WorkspaceSidebar.vue'
import WorkspaceSplitHandle from '@/components/WorkspaceSplitHandle.vue'
import SettingsPanel from '@/components/SettingsPanel.vue'
import InterruptModal from '@/components/InterruptModal.vue'
import { useSettingsStore } from '@/stores/settings'
import { useWorkspaceStore } from '@/stores/workspace'
import { useSessionStore } from '@/stores/sessions'
import { useCompletionsStore } from '@/stores/completions'
import { demoItems, demoInterrupts, demoSessions, demoSkills, demoSubagents, demoWorkspace } from './data'

const settings = useSettingsStore()
const workspace = useWorkspaceStore()
const sessions = useSessionStore()
const completions = useCompletionsStore()
const { t } = useI18n()

onMounted(() => {
  sessions.setDemo(true)
  sessions.seed(demoSessions, demoItems)
  workspace.setDemo(true)
  workspace.tree = demoWorkspace
  workspace.openPanel()
  completions.seed(demoSkills, demoSubagents)
})
</script>

<template>
  <div class="app-shell">
    <Sidebar />
    <main class="main">
      <div class="demo-banner">
        <span>woca demo · {{ t('demo.banner') }}</span>
        <div class="demo-actions">
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.permission)">
            {{ t('demo.permission') }}
          </button>
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.ask)">
            {{ t('demo.ask') }}
          </button>
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.plan)">
            {{ t('demo.plan') }}
          </button>
        </div>
      </div>
      <ChatPanel />
    </main>
    <WorkspaceSplitHandle v-if="workspace.panelOpen" />
    <WorkspaceSidebar />
    <SettingsPanel v-if="settings.panelOpen" />
    <InterruptModal v-if="sessions.activeInterrupt" />
  </div>
</template>

<style scoped>
.demo-banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 20px;
  font-size: 12px;
  color: var(--muted);
  background: var(--surface);
  border-bottom: 1px solid var(--border);
}
.demo-actions {
  display: flex;
  gap: 8px;
}
.demo-btn {
  padding: 5px 12px;
  font-size: 12px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  background: var(--surface);
  color: var(--text-2);
  transition: background 0.12s ease;
}
.demo-btn:hover {
  background: var(--surface-hover);
  color: var(--text);
}
</style>
