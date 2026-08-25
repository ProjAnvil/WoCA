<script setup lang="ts">
import { Radio } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useServerSettingsStore } from '../../stores/serverSettings'

const { t } = useI18n()
const settings = useServerSettingsStore()
</script>

<template>
  <!-- Message plane: how dispatched runs reach the workers -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Radio :size="13" style="vertical-align: -2px" /> {{ t('settings.msgplane.title') }}
    </div>

    <div v-if="!settings.server?.msg_plane_available" class="setting-row" style="display: block">
      <div class="row-hint" style="max-width: 520px">{{ t('settings.msgplane.unavailable') }}</div>
    </div>
    <div v-else class="setting-row" style="display: block">
      <div class="row-hint" style="max-width: 520px">{{ t('settings.msgplane.intro') }}</div>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.msgplane.mode') }}</div>
        <div class="row-hint">{{ t('settings.msgplane.modeHint') }}</div>
      </div>
      <select v-model="settings.msgPlaneMode" class="field" style="max-width: 240px">
        <option value="pg">{{ t('settings.msgplane.modePG') }}</option>
        <option value="pg+nats">{{ t('settings.msgplane.modePGNATS') }}</option>
      </select>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.msgplane.natsURL') }}</div>
        <div class="row-hint">{{ t('settings.msgplane.natsURLHint') }}</div>
      </div>
      <input
        v-model="settings.msgPlaneNATSURL"
        class="field"
        style="max-width: 260px"
        type="text"
        autocomplete="off"
        placeholder="nats://host:4222"
      />
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.msgplane.subject') }}</div>
        <div class="row-hint">{{ t('settings.msgplane.subjectHint') }}</div>
      </div>
      <input
        v-model="settings.msgPlaneSubject"
        class="field"
        style="max-width: 260px"
        type="text"
        autocomplete="off"
        placeholder="runs.dispatch"
      />
    </div>
  </section>
</template>
