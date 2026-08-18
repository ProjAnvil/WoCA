<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
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
      <input
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
