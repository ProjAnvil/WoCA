<script setup lang="ts">
import { Database } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
</script>

<template>
  <!-- Storage (checkpoint + memoryStore backends) -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Database :size="13" style="vertical-align: -2px" /> {{ t('settings.storage.title') }}
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.storage.checkpointBackend') }}</div>
        <div class="row-hint">{{ t('settings.storage.checkpointHint') }}</div>
      </div>
      <select v-model="settings.checkpointBackend" class="field" style="max-width: 200px">
        <option value="sqlite">{{ t('settings.storage.sqliteOption') }}</option>
        <option value="postgres">postgres</option>
        <option value="file">{{ t('settings.storage.fileOption') }}</option>
        <option value="none">none</option>
      </select>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.storage.memoryBackend') }}</div>
        <div class="row-hint">{{ t('settings.storage.memoryStoreHint') }}</div>
      </div>
      <select v-model="settings.memoryBackend" class="field" style="max-width: 200px">
        <option value="sqlite">{{ t('settings.storage.sqliteOption') }}</option>
        <option value="postgres">postgres</option>
        <option value="memory">{{ t('settings.storage.memoryOption') }}</option>
        <option value="none">none</option>
      </select>
    </div>

    <div class="setting-row">
      <div>
        <div class="row-label">Postgres DSN</div>
        <div class="row-hint">
          {{
            t('settings.storage.postgresHint', {
              status: settings.server?.postgres_url_set
                ? t('settings.shared.setEncrypted')
                : t('settings.shared.notSet'),
            })
          }}
        </div>
      </div>
      <input
        v-model="settings.postgresURL"
        class="field"
        style="max-width: 260px"
        type="password"
        autocomplete="off"
        placeholder="postgres://user:pass@host/db"
      />
    </div>

    <div v-if="settings.server" class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.storage.dbLocation') }}</div>
        <div class="row-hint">{{ settings.server.database }}</div>
      </div>
    </div>
  </section>
</template>
