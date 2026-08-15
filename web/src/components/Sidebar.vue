<script setup lang="ts">
import { Plus, Settings } from 'lucide-vue-next'
import SessionList from './SessionList.vue'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'

const sessions = useSessionStore()
const settings = useSettingsStore()
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar-brand">
      <div class="brand-mark">W</div>
      <span class="brand-name">woca</span>
    </div>

    <div class="sidebar-body">
      <section class="side-section">
        <div class="side-section-head">
          <span class="side-section-title">工作区</span>
          <button
            class="icon-btn"
            title="在当前工作区新建会话"
            @click="sessions.createSession()"
          >
            <Plus :size="16" />
          </button>
        </div>
        <SessionList />
      </section>
    </div>

    <div class="footer-bar">
      <button class="footer-settings-btn" @click="settings.openPanel()">
        <Settings :size="16" />
        <span>设置</span>
      </button>
    </div>
  </aside>
</template>
