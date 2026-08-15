<script setup lang="ts">
import { onMounted } from 'vue'
import Sidebar from '@/components/Sidebar.vue'
import ChatPanel from '@/components/ChatPanel.vue'
import SettingsPanel from '@/components/SettingsPanel.vue'
import MarkdownView from '@/components/MarkdownView.vue'
import InterruptModal from '@/components/InterruptModal.vue'
import { useSettingsStore } from '@/stores/settings'
import { useWorkspaceStore } from '@/stores/workspace'
import { useSessionStore } from '@/stores/sessions'
import { demoItems, demoInterrupts, demoSessions, demoWorkspace } from './data'

const settings = useSettingsStore()
const workspace = useWorkspaceStore()
const sessions = useSessionStore()

onMounted(() => {
  sessions.setDemo(true)
  sessions.seed(demoSessions, demoItems)
  workspace.tree = demoWorkspace
})
</script>

<template>
  <div class="app-shell">
    <Sidebar />
    <main class="main">
      <div class="demo-banner">
        <span>woca demo · 演示数据（未连接后端）</span>
        <div class="demo-actions">
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.permission)">
            权限审批
          </button>
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.ask)">提问</button>
          <button class="demo-btn" @click="sessions.triggerInterrupt(demoInterrupts.plan)">
            计划审批
          </button>
        </div>
      </div>
      <ChatPanel />
    </main>
    <SettingsPanel v-if="settings.panelOpen" />
    <MarkdownView v-if="workspace.preview" />
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
