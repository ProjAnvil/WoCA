<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'
import { useServerSettingsStore } from '../../stores/serverSettings'

const { t } = useI18n()
const settings = useSettingsStore()
const server = useServerSettingsStore()
</script>

<template>
  <!-- Preferences -->
  <section class="setting-section">
    <div class="setting-section-title">{{ t('settings.preferences.title') }}</div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.preferences.autoCollapse') }}</div>
        <div class="row-hint">{{ t('settings.preferences.autoCollapseHint') }}</div>
      </div>
      <button
        class="switch"
        role="switch"
        :class="{ on: settings.autoCollapseTools }"
        :aria-label="t('settings.preferences.autoCollapse')"
        :aria-checked="settings.autoCollapseTools"
        @click="settings.autoCollapseTools = !settings.autoCollapseTools"
      ></button>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.preferences.markdown') }}</div>
        <div class="row-hint">{{ t('settings.preferences.markdownHint') }}</div>
      </div>
      <button
        class="switch"
        role="switch"
        :class="{ on: settings.markdownPreview }"
        :aria-label="t('settings.preferences.markdown')"
        :aria-checked="settings.markdownPreview"
        @click="settings.markdownPreview = !settings.markdownPreview"
      ></button>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.preferences.debugLog') }}</div>
        <div class="row-hint">
          {{
            t('settings.preferences.debugLogHint', {
              dir: server.logDir ? `· ${server.logDir}` : '',
            })
          }}
        </div>
      </div>
      <button
        class="switch"
        role="switch"
        :class="{ on: server.debugLog }"
        :aria-label="t('settings.preferences.debugLog')"
        :aria-checked="server.debugLog"
        :title="t('settings.preferences.debugLogTitle')"
        @click="server.toggleDebugLog()"
      ></button>
    </div>
  </section>
</template>
