<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import * as api from '../../api/client'
import type { StatsResponse } from '../../types'

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
    <h3>运行状态</h3>
    <div v-if="data" class="stats-grid">
      <span class="stat-chip">活跃 run：{{ data.active_runs }}</span>
      <span class="stat-chip">排队 run：{{ data.queued_runs }}</span>
      <span class="stat-chip">teammate：{{ data.teammates }}</span>
      <span class="stat-chip">后台 task：{{ data.background_tasks }}</span>
      <span class="stat-chip">SSE 丢弃：{{ data.sse_dropped }}</span>
      <span class="stat-chip">LSP：{{ data.lsp.active }} 活跃 / {{ data.lsp.broken }} 熔断</span>
    </div>
    <p v-else>加载中…</p>
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
