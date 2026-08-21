<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import * as api from '../../api/client'
import type { StatsResponse } from '../../types'

const { t } = useI18n()
const data = ref<StatsResponse | null>(null)
const grafanaUrl = ref('')
let timer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  data.value = await api.stats()
}

onMounted(() => {
  void refresh()
  // Ops console link (server editions only): fetched once, hidden when the
  // deployment exposes no Grafana URL.
  void api.health().then((h) => {
    grafanaUrl.value = h.grafana_url ?? ''
  }).catch(() => {})
  timer = setInterval(() => void refresh(), 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <section>
    <h3>{{ t('settings.stats.title') }}</h3>
    <div v-if="data" class="stats-grid">
      <span class="stat-chip">{{ t('settings.stats.activeRuns', { count: data.active_runs }) }}</span>
      <span class="stat-chip">{{ t('settings.stats.queuedRuns', { count: data.queued_runs }) }}</span>
      <span class="stat-chip">{{ t('settings.stats.teammates', { count: data.teammates }) }}</span>
      <span class="stat-chip">{{ t('settings.stats.backgroundTasks', { count: data.background_tasks }) }}</span>
      <span class="stat-chip">{{ t('settings.stats.sseDropped', { count: data.sse_dropped }) }}</span>
      <span class="stat-chip">
        {{ t('settings.stats.lsp', { active: data.lsp.active, broken: data.lsp.broken }) }}
      </span>
    </div>
    <p v-else>{{ t('common.loading') }}</p>
    <a v-if="grafanaUrl" class="ops-link" :href="grafanaUrl" target="_blank" rel="noopener">
      <span class="ops-title">{{ t('settings.stats.opsLink') }}</span>
      <span class="ops-hint">{{ t('settings.stats.opsHint') }}</span>
    </a>
  </section>
</template>

<style scoped>
.ops-link {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 14px;
  padding: 10px 12px;
  border: 1px solid var(--border, #2a2a2e);
  border-radius: 8px;
  text-decoration: none;
  color: inherit;
}
.ops-link:hover {
  border-color: var(--accent, #4c8bf5);
}
.ops-title {
  font-weight: 600;
}
.ops-hint {
  font-size: 12px;
  opacity: 0.7;
}
</style>

<style scoped>
.stats-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.stat-chip {
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--bg-soft, rgba(127, 127, 127, 0.12));
  font-size: 12px;
}
</style>
