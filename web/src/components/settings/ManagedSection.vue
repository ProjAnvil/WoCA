<script setup lang="ts">
import { onMounted } from 'vue'
import { CloudDownload } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import type { BridgeStatus } from '../../api/client'
import { useManagedStore } from '../../stores/sections'

const { t } = useI18n()
const managedStore = useManagedStore()
const managed = managedStore.state

// The cloud-channel snapshot rides along for the sync line (config
// version, last pull, STALE lamp) — it is the same data the cloud
// access page shows, read from /api/status by the panel.
defineProps<{ status?: BridgeStatus | null }>()

onMounted(() => {
  if (!managed.loaded) void managedStore.load()
})
</script>

<template>
  <!-- 远端配置: the config-plane delivery, read-only. It materializes as
       the lowest merge layer — the user's own config always wins. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <CloudDownload :size="13" style="vertical-align: -2px" /> {{ t('settings.managed.title') }}
    </div>
    <p class="mem-intro">{{ t('settings.managed.intro') }}</p>

    <div v-if="status?.cloud?.enrolled" class="setting-subgroup managed-sync" data-testid="managed-sync">
      <span class="row-hint">
        {{ t('settings.managed.version') }}: <code>{{ status.cloud.last_version || '—' }}</code>
        · {{ t('settings.managed.lastPull') }}: {{ status.cloud.last_pull || '—' }}
      </span>
      <span v-if="status.cloud.stale" class="managed-stale">{{ t('settings.managed.stale') }}</span>
    </div>

    <div v-if="managed.loading" class="row-hint">{{ t('settings.managed.loading') }}</div>
    <div v-else-if="managed.error" class="row-hint" style="color: var(--danger)">
      {{ managed.error }}
    </div>

    <template v-if="managed.delivery">
      <div class="setting-subgroup">
        <div class="row-label">
          {{ t('settings.managed.skillsLabel', { count: managed.delivery.skills.length }) }}
        </div>
        <div v-if="!managed.delivery.skills.length" class="row-hint">
          {{ t('settings.managed.noSkills') }}
        </div>
        <div v-else class="managed-list">
          <div v-for="s in managed.delivery.skills" :key="s.name" class="managed-row" :data-testid="`managed-skill-${s.name}`">
            <span class="managed-name">{{ s.name }}</span>
            <span class="managed-desc" :title="s.description">{{ s.description }}</span>
          </div>
        </div>
      </div>

      <div class="setting-subgroup">
        <div class="row-label">
          {{ t('settings.managed.mcpLabel', { count: managed.delivery.mcp.length }) }}
        </div>
        <div v-if="!managed.delivery.mcp.length" class="row-hint">
          {{ t('settings.managed.noMcp') }}
        </div>
        <div v-else class="managed-list">
          <div v-for="m in managed.delivery.mcp" :key="m.name" class="managed-row" :data-testid="`managed-mcp-${m.name}`">
            <span class="managed-name">{{ m.name }}</span>
            <span class="managed-desc" :title="m.url || m.command">
              {{ m.type }} · {{ m.url || m.command }}
            </span>
          </div>
        </div>
      </div>

      <p class="row-hint">{{ t('settings.managed.readonlyNote') }}</p>
    </template>
  </section>
</template>

<style scoped>
.managed-sync {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
}
.managed-sync code {
  font-family: var(--font-mono);
  font-size: 12px;
}
.managed-stale {
  font-size: 12px;
  color: var(--warn);
}
.managed-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 6px;
  max-height: 320px;
  overflow-y: auto;
}
.managed-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius);
}
.managed-row:hover {
  background: var(--surface-hover);
}
.managed-name {
  flex: 0 0 auto;
  font-size: 13px;
  font-family: var(--font-mono);
}
.managed-desc {
  flex: 1 1 auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  color: var(--faint);
}
</style>
