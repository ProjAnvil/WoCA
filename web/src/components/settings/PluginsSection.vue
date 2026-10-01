<script setup lang="ts">
import WSkeleton from '../ui/WSkeleton.vue'
import { computed, onMounted } from 'vue'
import { Activity, Puzzle } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useServerSettingsStore } from '../../stores/serverSettings'
import { usePluginsStore } from '../../stores/sections'
import WInfoTip from '../ui/WInfoTip.vue'

// bridge=true on the private-cloud form: the same dual track as skills/MCP
// applies — the organization can pin the enabled set via the config plane
// (the builtin-plugins managed lock), and whatever it leaves open stays
// the user's own choice.
defineProps<{ bridge?: boolean }>()

const { t } = useI18n()
const settings = useServerSettingsStore()
const plugins = usePluginsStore()
const pluginState = plugins.state
const orgLocked = computed(() => settings.isManaged('builtin-plugins'))
// The Langfuse observability plugin is a personal-form surface: enterprise
// deployments own it from the console (the server reports
// langfuse_console_managed there and rejects writes).
const langfuseHidden = computed(() => settings.langfuseConsoleManaged)

// Entering the section always refreshes (not just the first visit): the
// install-progress poller pauses while the section is unmounted, and the
// store's loading guard would leave "installing" stuck at its stale state.
onMounted(() => {
  void plugins.load()
})

const kindLabels = computed<Record<string, string>>(() => ({
  golang: t('settings.plugins.kindGolang'),
  python: 'Python · uv',
}))

const stateLabels = computed<Record<string, string>>(() => ({
  not_installed: t('settings.plugins.stateNotInstalled'),
  installing: t('settings.plugins.stateInstalling'),
  ready: t('settings.plugins.stateReady'),
  error: t('settings.plugins.stateError'),
}))
</script>

<template>
  <!-- Built-in capability plugins: enable toggles + runtime installs -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Puzzle :size="13" style="vertical-align: -2px" /> {{ t('settings.plugins.title') }}
    </div>
    <p class="mem-intro">
      {{
        bridge
          ? orgLocked
            ? t('settings.plugins.orgManagedIntro')
            : t('settings.plugins.bridgeIntro')
          : t('settings.plugins.intro')
      }}
    </p>

    <div v-if="pluginState.loading && !pluginState.loaded" role="status" aria-busy="true">
      <span class="sr-only">{{ t('common.loading') }}</span>
      <div v-for="i in 3" :key="i" class="plugin-skeleton">
        <WSkeleton variant="rect" height="44px" />
      </div>
    </div>
    <div v-else-if="pluginState.error" class="row-hint" style="color: var(--danger)">
      {{ pluginState.error }}
    </div>
    <div v-if="pluginState.message" class="row-hint" style="margin-bottom: 6px">
      {{ pluginState.message }}
    </div>

    <div v-for="p in pluginState.plugins" :key="p.name" class="plugin-card">
      <div class="plugin-card-head">
        <div>
          <span class="env-check-name">{{ p.name }}</span>
          <span class="plugin-badge">{{ kindLabels[p.kind] ?? p.kind }}</span>
          <span v-if="p.version" class="plugin-badge plugin-badge-dim">{{ p.version }}</span>
          <span v-if="orgLocked && p.enabled" class="plugin-badge plugin-badge-org">
            {{ t('settings.plugins.orgBadge') }}
          </span>
          <span class="plugin-state" :data-state="p.state">
            {{ stateLabels[p.state] ?? p.state }}
          </span>
        </div>
        <button
          class="switch"
          role="switch"
          :class="{ on: p.enabled }"
          :disabled="pluginState.toggling === p.name || orgLocked"
          :aria-label="p.name"
          :aria-checked="p.enabled"
          :title="orgLocked ? t('settings.managedTooltip') : p.enabled ? t('settings.plugins.disableTip') : t('settings.plugins.enableTip')"
          @click="plugins.toggle(p)"
        ></button>
      </div>
      <div class="row-hint">{{ p.description }}</div>
      <div class="row-hint">
        {{ t('settings.plugins.tools') }}<code v-for="t in p.tools" :key="t" class="plugin-tool">{{ t }}</code>
      </div>
      <div v-if="p.state === 'ready' && p.detail" class="row-hint plugin-versions">
        {{ p.detail }}
      </div>
      <div v-if="p.state === 'error' && p.detail" class="row-hint plugin-error">
        {{ p.detail }}
      </div>
      <div v-if="p.log?.length" class="plugin-log">
        <div v-for="(line, i) in p.log.slice(-8)" :key="i">{{ line }}</div>
      </div>
      <div class="plugin-actions">
        <button
          v-if="p.state === 'ready' && p.runtime"
          class="btn"
          :title="t('settings.plugins.reinstallTip')"
          @click="plugins.install(p, true)"
        >
          {{ t('settings.plugins.reinstall') }}
        </button>
        <button
          v-if="p.state !== 'ready' && p.runtime"
          class="btn"
          :disabled="p.state === 'installing'"
          @click="plugins.install(p, false)"
        >
          {{ p.state === 'installing' ? t('settings.plugins.stateInstalling') : t('settings.plugins.downloadInstall') }}
        </button>
      </div>
    </div>

    <!-- Langfuse observability plugin: spans follow the OTel GenAI
         conventions; the plugin exports them over OTLP -->
    <div v-if="!langfuseHidden" class="plugin-card" data-testid="langfuse-card">
      <div class="plugin-card-head">
        <div>
          <Activity :size="13" style="vertical-align: -2px" />
          <span class="env-check-name" style="margin-left: 4px">{{ t('settings.plugins.langfuse.title') }}</span>
          <span class="plugin-badge">OTLP</span>
        </div>
        <button
          class="switch"
          role="switch"
          :class="{ on: settings.langfuseEnabled }"
          :aria-label="t('settings.plugins.langfuse.title')"
          :aria-checked="settings.langfuseEnabled"
          :title="settings.langfuseEnabled ? t('settings.plugins.langfuse.disableTip') : t('settings.plugins.langfuse.enableTip')"
          @click="settings.langfuseEnabled = !settings.langfuseEnabled"
        ></button>
      </div>
      <div class="row-hint">{{ t('settings.plugins.langfuse.intro') }}</div>
      <template v-if="settings.langfuseEnabled">
        <div class="setting-row">
          <div>
            <div class="row-label">{{ t('settings.plugins.langfuse.baseURL') }}<WInfoTip :text="t('settings.plugins.langfuse.baseURLHint')" /></div>
          </div>
          <input
            v-model="settings.langfuseBaseURL"
            class="field"
            style="max-width: 280px"
            type="text"
            autocomplete="off"
            placeholder="https://cloud.langfuse.com"
          />
        </div>
        <div class="setting-row">
          <div>
            <div class="row-label">{{ t('settings.plugins.langfuse.publicKey') }}</div>
          </div>
          <input
            v-model="settings.langfusePublicKey"
            class="field"
            style="max-width: 280px"
            type="text"
            autocomplete="off"
            placeholder="pk-lf-…"
          />
        </div>
        <div class="setting-row">
          <div>
            <div class="row-label">{{ t('settings.plugins.langfuse.secretKey') }}</div>
          </div>
          <input
            v-model="settings.langfuseSecret"
            class="field"
            style="max-width: 280px"
            type="password"
            autocomplete="new-password"
            :placeholder="settings.langfuseSecretSet ? t('settings.plugins.langfuse.secretSet') : 'sk-lf-…'"
          />
        </div>
        <div class="row-hint">{{ t('settings.plugins.langfuse.restartHint') }}</div>
      </template>
    </div>
  </section>
</template>

<style scoped>
/* ---------------- plugin cards ---------------- */

.plugin-card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.plugin-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.plugin-badge {
  display: inline-block;
  margin-left: 8px;
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--border);
  font-size: 11px;
  color: var(--faint);
  vertical-align: 1px;
}
.plugin-badge-dim {
  color: var(--faint);
}
.plugin-badge-org {
  border-color: var(--focus);
  color: var(--focus);
}
.plugin-state {
  margin-left: 8px;
  font-size: 11px;
  vertical-align: 1px;
  color: var(--faint);
}
.plugin-state[data-state='ready'] {
  color: var(--ok-text);
}
.plugin-state[data-state='installing'] {
  color: var(--warn);
}
.plugin-state[data-state='error'] {
  color: var(--danger);
}
.plugin-tool {
  display: inline-block;
  margin: 1px 4px 1px 0;
  padding: 1px 6px;
  border-radius: var(--radius);
  background: var(--surface-hover);
  font-size: 11px;
}
.plugin-error {
  color: var(--danger);
  white-space: pre-wrap;
  word-break: break-all;
}
.plugin-log {
  max-height: 140px;
  overflow-y: auto;
  border-radius: var(--radius);
  background: var(--surface-hover);
  padding: 6px 8px;
  font-size: 11px;
  font-family: var(--font-mono);
  color: var(--faint);
  white-space: pre-wrap;
  word-break: break-all;
}
.plugin-actions {
  display: flex;
  gap: 8px;
}
.plugin-versions {
  color: var(--ok-text);
}
</style>
