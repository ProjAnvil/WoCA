<script setup lang="ts">
import { computed } from 'vue'
import { Server } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useServerSettingsStore } from '../../stores/serverSettings'

defineProps<{ locked?: boolean }>()

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
       catalog (模型区) is the selection surface and keys are delivered by
       the config plane, so the fields gray out instead of failing on save. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Server :size="13" style="vertical-align: -2px" /> {{ t('settings.provider.title') }}
    </div>
    <p v-if="locked" class="mem-intro">{{ t('settings.provider.managedNotice') }}</p>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.provider.provider') }}</div>
        <div class="row-hint">{{ t('settings.provider.providerHint') }}</div>
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
        <div class="row-label">{{ t('settings.provider.defaultModel') }}</div>
        <div class="row-hint">{{ locked ? t('settings.provider.modelCatalogHint') : t('settings.shared.freeModelHint') }}</div>
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
