<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import * as api from '../../api/client'
import type { StatsResponse } from '../../types'

const { t } = useI18n()
const data = ref<StatsResponse | null>(null)
let timer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  data.value = await api.stats()
}

onMounted(() => {
  void refresh()
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
  </section>
</template>

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
