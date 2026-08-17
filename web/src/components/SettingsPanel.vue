<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import {
  Brain,
  Cpu,
  Database,
  Palette,
  Plug,
  Puzzle,
  Server,
  Settings2,
  X,
} from 'lucide-vue-next'
import { useSettingsStore } from '../stores/settings'
import AppearanceSection from './settings/AppearanceSection.vue'
import PreferencesSection from './settings/PreferencesSection.vue'
import ProviderSection from './settings/ProviderSection.vue'
import ClaudeEnvSection, { resetClaudeEnv } from './settings/ClaudeEnvSection.vue'
import PluginsSection, { resetPluginState } from './settings/PluginsSection.vue'
import StorageSection from './settings/StorageSection.vue'
import MemorySection from './settings/MemorySection.vue'
import ModelSection from './settings/ModelSection.vue'

const settings = useSettingsStore()

// Sidebar navigation: each entry maps to one section of the right pane.
const sections = [
  { id: 'appearance', label: '外观', icon: Palette },
  { id: 'preferences', label: '偏好', icon: Settings2 },
  { id: 'provider', label: '模型服务商', icon: Server },
  { id: 'claudeenv', label: '技能与 MCP', icon: Plug },
  { id: 'plugins', label: '插件', icon: Puzzle },
  { id: 'storage', label: '存储', icon: Database },
  { id: 'memory', label: '记忆', icon: Brain },
  { id: 'model', label: '模型', icon: Cpu },
]

const active = ref<string>('appearance')

// Suggested model names — the inputs are free-form; these are datalist hints.
const modelSuggestions = [
  'claude-sonnet-4-5',
  'claude-haiku-4-5',
  'claude-opus-4-5',
  'gpt-5.1',
  'gpt-4o',
  'gpt-4o-mini',
  'glm-4.6',
  'glm-5',
  'deepseek-chat',
  'deepseek-reasoner',
]

// The claudeenv / plugins sections keep their fetch state at module scope so
// it survives section switches; closing the panel must reset it, matching the
// lifetime that state had when it lived in this component's setup scope.
onBeforeUnmount(() => {
  resetClaudeEnv()
  resetPluginState()
})
</script>

<template>
  <div class="overlay" @click.self="settings.closePanel()">
    <div class="card settings-card">
      <div class="card-head">
        <span class="card-title">设置</span>
        <button class="icon-btn" @click="settings.closePanel()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body settings-layout">
        <!-- Left sidebar: section navigation -->
        <nav class="settings-nav">
          <button
            v-for="s in sections"
            :key="s.id"
            class="settings-nav-item"
            :class="{ active: active === s.id }"
            @click="active = s.id"
          >
            <component :is="s.icon" :size="14" />
            <span>{{ s.label }}</span>
          </button>
        </nav>

        <!-- Right pane: the selected section's fields -->
        <div class="settings-content">
          <AppearanceSection v-if="active === 'appearance'" />
          <PreferencesSection v-if="active === 'preferences'" />
          <ProviderSection v-if="active === 'provider'" />
          <ClaudeEnvSection v-if="active === 'claudeenv'" />
          <PluginsSection v-if="active === 'plugins'" />
          <StorageSection v-if="active === 'storage'" />
          <MemorySection v-if="active === 'memory'" />
          <ModelSection v-if="active === 'model'" />

          <!-- Shared datalist for the free-form model inputs -->
          <datalist id="woca-model-suggestions">
            <option v-for="m in modelSuggestions" :key="m" :value="m" />
          </datalist>
        </div>
      </div>

      <!-- Sticky footer: server-settings save actions -->
      <div class="settings-footer">
        <span v-if="settings.serverError" class="row-hint" style="color: var(--danger, #e5484d)">
          {{ settings.serverError }}
        </span>
        <span v-else-if="settings.savedMessage" class="row-hint">{{ settings.savedMessage }}</span>
        <span v-else class="row-hint">服务商/存储设置保存到数据库，密钥加密存储</span>
        <span v-if="settings.restartRequired" class="row-hint" style="color: var(--warn, #f5a524)">
          后端变更将在重启 woca 后生效
        </span>
        <button class="btn" :disabled="settings.saving" @click="settings.saveServer()">
          {{ settings.saving ? '保存中…' : '保存到数据库' }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.settings-card {
  width: min(880px, 94vw);
  height: min(640px, 86vh);
  max-width: 100%;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

/* Two-pane settings layout: left section nav, right scrollable fields. */
.settings-layout {
  display: flex;
  gap: 0;
  padding: 0;
  min-height: 0;
  flex: 1;
  overflow: hidden;
}

.settings-nav {
  flex: 0 0 168px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 14px 10px;
  border-right: 1px solid var(--border);
}

.settings-nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border: none;
  border-radius: var(--radius);
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  text-align: left;
  cursor: pointer;
  transition: background 0.12s ease, color 0.12s ease;
}
.settings-nav-item:hover {
  background: var(--surface-2);
  color: var(--text);
}
.settings-nav-item.active {
  background: var(--surface-2);
  color: var(--text);
  font-weight: 600;
  box-shadow: inset 2px 0 0 var(--focus);
}

.settings-content {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: 6px 24px 16px;
}

/* Sticky save bar shared by the server-backed sections. */
.settings-footer {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  padding: 12px 20px;
  border-top: 1px solid var(--border);
  background: var(--surface);
}
</style>
