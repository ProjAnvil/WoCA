<script setup lang="ts">
import { computed } from 'vue'
import { Server } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useServerSettingsStore } from '../../stores/serverSettings'
import WInfoTip from '../ui/WInfoTip.vue'

defineProps<{ locked?: boolean }>()
const emit = defineEmits<{ navigate: [section: string] }>()

const { t } = useI18n()
const settings = useServerSettingsStore()

// Per-provider defaults shown as input placeholders.
const providerDefaults = computed<Record<string, { base: string; key: string }>>(() => ({
  anthropic: { base: 'https://api.anthropic.com', key: 'sk-ant-…' },
  openai: { base: 'https://api.openai.com/v1', key: 'sk-…' },
  glm: { base: `https://open.bigmodel.cn/api/anthropic${t('settings.provider.defaultTag')}`, key: t('settings.provider.glmKeyHint') },
  deepseek: { base: `https://api.deepseek.com/anthropic${t('settings.provider.defaultTag')}`, key: 'deepseek-…' },
}))
const providerHint = computed(
  () => providerDefaults.value[settings.provider] ?? providerDefaults.value.anthropic
)
</script>

<template>
  <!-- Provider (persisted server-side; keys encrypted at rest).
       One Base URL + one API Key field target the selected provider.
       In the managed bridge form the whole domain is cloud-governed: the
       catalog (model section) is the selection surface and keys are delivered by
       the config plane, so the fields gray out instead of failing on save. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Server :size="13" style="vertical-align: -2px" /> {{ t('settings.provider.title') }}
      <span v-if="locked" class="managed-badge">{{ t('settings.managedBadge') }}</span>
    </div>
    <p v-if="locked" class="mem-intro">
      {{ t('settings.provider.managedNotice') }}
      <button type="button" class="managed-cta" @click="emit('navigate', 'model')">
        {{ t('settings.provider.managedCta') }}
      </button>
    </p>

    <div class="setting-row">
      <div>
<div class="row-label">{{ t('settings.provider.provider') }}<WInfoTip :text="t('settings.provider.providerHint')" /></div>
      </div>
      <select
        v-model="settings.provider"
        class="field"
        style="max-width: 200px"
        :disabled="locked || settings.isManaged('provider')"
        :title="locked || settings.isManaged('provider') ? t('settings.managedTooltip') : undefined"
      >
        <option value="anthropic">anthropic</option>
        <option value="openai">openai</option>
        <option value="glm">{{ t('settings.provider.glmOption') }}</option>
        <option value="deepseek">deepseek</option>
      </select>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">Base URL</div>
        <div class="row-hint">
          {{ t('settings.provider.baseUrlHint', { provider: settings.provider }) }}
        </div>
      </div>
      <input
        v-model="settings.baseURL"
        class="field"
        style="max-width: 260px"
        :disabled="locked"
        :placeholder="providerHint.base"
      />
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">API Key</div>
        <div class="row-hint">
          {{
            t('settings.provider.apiKeyHint', {
              status: settings.apiKeySet
                ? t('settings.shared.setEncrypted')
                : t('settings.shared.notSet'),
              provider: settings.provider,
            })
          }}
        </div>
      </div>
      <input
        v-model="settings.apiKey"
        class="field"
        style="max-width: 260px"
        type="password"
        autocomplete="off"
        :disabled="locked"
        :placeholder="providerHint.key"
      />
    </div>

    <div class="setting-row">
      <div>
<div class="row-label">{{ t('settings.provider.defaultModel') }}<WInfoTip :text="locked ? t('settings.provider.modelCatalogHint') : t('settings.shared.freeModelHint')" /></div>
      </div>
      <input
        v-model="settings.serverModel"
        class="field"
        style="max-width: 260px"
        list="woca-model-suggestions"
        placeholder="claude-sonnet-4-5"
        :disabled="locked"
      />
    </div>
  </section>
</template>

<style scoped>
/* Org-managed badge (locked domains): who owns this section. */
.managed-badge {
  margin-left: 8px;
  padding: 0 8px;
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 10.5px;
  font-weight: 400;
  color: var(--text-2);
  vertical-align: 1px;
}
/* "Where to pick models" — the locked provider's only actionable path. */
.managed-cta {
  border: none;
  background: none;
  padding: 0;
  margin-left: 6px;
  color: var(--link);
  font: inherit;
  font-size: 12.5px;
  cursor: pointer;
}
.managed-cta:hover {
  text-decoration: underline;
}
</style>
