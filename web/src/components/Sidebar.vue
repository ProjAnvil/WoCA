<script setup lang="ts">
import { nextTick, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Plus, Settings, X } from 'lucide-vue-next'
import SessionList from './SessionList.vue'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'
import { baseName } from '../lib/format'

const { t } = useI18n()
const sessions = useSessionStore()
const settings = useSettingsStore()

// The header "+" adds a new workspace (a directory path; the workspace
// materializes in the list via its first session). Adding a session to an
// existing workspace is the "+" on each workspace group in SessionList.
const wsDialogOpen = ref(false)
const wsPath = ref('')
const wsInput = ref<HTMLInputElement | null>(null)

function openWorkspaceDialog() {
  wsPath.value = ''
  wsDialogOpen.value = true
  nextTick(() => wsInput.value?.focus())
}

async function createWorkspace() {
  const path = wsPath.value.trim().replace(/\/+$/, '')
  if (!path) return
  wsDialogOpen.value = false
  await sessions.createSession({ path, name: baseName(path) })
}
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
          <span class="side-section-title">{{ t('sidebar.workspaces') }}</span>
          <button class="icon-btn" :title="t('sidebar.newWorkspace')" @click="openWorkspaceDialog">
            <Plus :size="16" />
          </button>
        </div>
        <SessionList />
      </section>
    </div>

    <div class="footer-bar">
      <button class="footer-settings-btn" @click="settings.openPanel()">
        <Settings :size="16" />
        <span>{{ t('sidebar.settings') }}</span>
      </button>
    </div>

    <div v-if="wsDialogOpen" class="overlay" @click.self="wsDialogOpen = false">
      <div class="card ws-dialog">
        <div class="card-head">
          <span class="card-title">{{ t('sidebar.newWorkspace') }}</span>
          <button class="icon-btn" :title="t('common.close')" @click="wsDialogOpen = false">
            <X :size="15" />
          </button>
        </div>
        <div class="card-body">
          <label class="field-label" for="ws-path-input">{{ t('sidebar.workspacePathLabel') }}</label>
          <input
            id="ws-path-input"
            ref="wsInput"
            v-model="wsPath"
            class="field"
            type="text"
            :placeholder="t('sidebar.pathPlaceholder')"
            @keydown.enter="createWorkspace"
            @keydown.esc="wsDialogOpen = false"
          />
          <p class="ws-dialog-hint">{{ t('sidebar.newWorkspaceHint') }}</p>
        </div>
        <div class="interrupt-actions">
          <button class="btn" @click="wsDialogOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" :disabled="!wsPath.trim()" @click="createWorkspace">
            {{ t('sidebar.create') }}
          </button>
        </div>
      </div>
    </div>
  </aside>
</template>
