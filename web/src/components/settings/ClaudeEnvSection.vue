<script setup lang="ts">
import { onMounted } from 'vue'
import { Plug } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useClaudeEnvStore } from '../../stores/sections'

// bridge=true on the private-cloud execution plane: the ~/.claude that gets
// scanned is the BRIDGE machine's own (this page also works through the
// edge tunnel from the public web — the import runs where the agent runs).
defineProps<{ bridge?: boolean }>()

const { t } = useI18n()
const claudeEnv = useClaudeEnvStore()

// Load on first activation of the section (mount); later activations keep
// the store's state until the settings panel closes and resets it.
onMounted(() => {
  if (!claudeEnv.state.loaded) void claudeEnv.load()
})
</script>

<template>
  <!-- Claude environment: ~/.claude skills + global MCP servers,
       checkbox-selected per agent start. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Plug :size="13" style="vertical-align: -2px" /> {{ t('settings.claudeenv.title') }}
    </div>
    <p class="mem-intro">{{ bridge ? t('settings.claudeenv.bridgeIntro') : t('settings.claudeenv.intro') }}</p>

    <div v-if="claudeEnv.state.loading" class="row-hint">{{ t('settings.claudeenv.detecting') }}</div>
    <div v-else-if="claudeEnv.state.error" class="row-hint" style="color: var(--danger, #e5484d)">
      {{ claudeEnv.state.error }}
    </div>

    <template v-if="claudeEnv.state.env">
      <div class="setting-subgroup">
        <div class="row-label">
          {{
            t('settings.claudeenv.skillsLabel', {
              selected: claudeEnv.state.env.selected.skills.length,
              total: claudeEnv.state.env.skills.length,
            })
          }}
        </div>
        <div class="row-hint">{{ t('settings.claudeenv.skillsHint') }}</div>
        <div v-if="!claudeEnv.state.env.skills.length" class="row-hint">
          {{ t('settings.claudeenv.noSkills') }}
        </div>
        <div v-else class="env-check-list">
          <label v-for="s in claudeEnv.state.env.skills" :key="s.name" class="env-check">
            <input
              type="checkbox"
              :checked="claudeEnv.state.env.selected.skills.includes(s.name)"
              @change="claudeEnv.toggleSkill(s.name)"
            />
            <span class="env-check-name">{{ s.name }}</span>
            <span class="env-check-desc" :title="s.description">{{ s.description }}</span>
          </label>
        </div>
      </div>

      <div class="setting-subgroup">
        <div class="row-label">
          {{
            t('settings.claudeenv.mcpLabel', {
              selected: claudeEnv.state.env.selected.mcp.length,
              total: claudeEnv.state.env.mcp.length,
            })
          }}
        </div>
        <div class="row-hint">{{ t('settings.claudeenv.mcpHint') }}</div>
        <div v-if="!claudeEnv.state.env.mcp.length" class="row-hint">
          {{ t('settings.claudeenv.noMcp') }}
        </div>
        <div v-else class="env-check-list">
          <label v-for="m in claudeEnv.state.env.mcp" :key="m.name" class="env-check">
            <input
              type="checkbox"
              :checked="claudeEnv.state.env.selected.mcp.includes(m.name)"
              @change="claudeEnv.toggleMCP(m.name)"
            />
            <span class="env-check-name">{{ m.name }}</span>
            <span class="env-check-desc" :title="m.url || m.command">
              {{ m.type }} · {{ m.url || m.command }}{{ m.has_env ? ' · ' + t('settings.claudeenv.hasEnv') : '' }}
            </span>
          </label>
        </div>
      </div>

      <div class="setting-row">
        <button class="btn" :disabled="claudeEnv.state.saving" @click="claudeEnv.save()">
          {{ claudeEnv.state.saving ? t('settings.shared.saving') : t('settings.claudeenv.save') }}
        </button>
        <span v-if="claudeEnv.state.message" class="row-hint" style="margin-left: 10px">{{ claudeEnv.state.message }}</span>
      </div>
    </template>
  </section>
</template>

<style scoped>
/* Skill & MCP checkbox list */
.env-check-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 6px;
  max-height: 320px;
  overflow-y: auto;
}
.env-check {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius);
  cursor: pointer;
}
.env-check:hover {
  background: var(--surface-hover);
}
.env-check input[type='checkbox'] {
  flex: 0 0 auto;
  transform: translateY(1px);
  accent-color: var(--focus);
}
.env-check-desc {
  flex: 1 1 auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  color: var(--faint);
}
</style>
