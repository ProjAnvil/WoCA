<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
const appliedId = ref('')

async function onPick(e: Event) {
  const id = (e.target as HTMLSelectElement).value
  appliedId.value = id
  await settings.applyModel(id)
}

onMounted(() => {
  void settings.ensureModels()
  if (settings.models.length) appliedId.value = settings.models[0].id
})
</script>

<template>
  <!-- Model (session preferences) -->
  <section class="setting-section">
    <div class="setting-section-title">{{ t('settings.model.title') }}</div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.model.defaultModel') }}</div>
        <div class="row-hint">{{ t('settings.shared.freeModelHint') }}</div>
      </div>
      <!-- Private-cloud form (managed): the operator's preset catalog is
           the ONLY source — selecting one applies it (the bridge asks the
           catalog authority to verify; unknown models are refused). An
           empty catalog disables selection entirely and the composer
           refuses to send. -->
      <div v-if="settings.serverError" class="row-hint" style="color: var(--danger, #e5484d); max-width: 260px">
        {{ settings.serverError }}
      </div>
      <select
        v-if="settings.models.length"
        :value="appliedId"
        class="field"
        style="max-width: 260px"
        :disabled="settings.applyingModel"
        @change="onPick"
      >
        <option v-for="m in settings.models" :key="m.id" :value="m.id">
          {{ m.name }} ({{ m.model }})
        </option>
      </select>
      <div v-else-if="settings.noModelsManaged" class="row-hint" style="max-width: 260px">
        {{ t('settings.model.noCatalog') }}
      </div>
      <!-- Standalone form: config-plane allowlist when present, else free
           input (product design §3.1). -->
      <select
        v-else-if="settings.modelWhitelist.length"
        v-model="settings.model"
        class="field"
        style="max-width: 260px"
      >
        <option v-for="m in settings.modelWhitelist" :key="m" :value="m">{{ m }}</option>
      </select>
      <input
        v-else
        v-model="settings.model"
        class="field"
        style="max-width: 260px"
        list="woca-model-suggestions"
        placeholder="claude-sonnet-4-5"
      />
    </div>

    <div class="setting-row">
      <div class="row-label">{{ t('settings.model.permissionMode') }}</div>
      <select v-model="settings.permissionMode" class="field" style="max-width: 200px">
        <option value="default">default</option>
        <option value="acceptEdits">acceptEdits</option>
        <option value="bypassPermissions">bypassPermissions</option>
        <option value="readOnly">readOnly</option>
        <option value="plan">plan</option>
      </select>
    </div>
  </section>
</template>
