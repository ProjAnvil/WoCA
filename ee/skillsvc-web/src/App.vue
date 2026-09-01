<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AssetList from './components/AssetList.vue'
import VersionPanel from './components/VersionPanel.vue'
import LabelPanel from './components/LabelPanel.vue'

const { locale } = useI18n()
const assets = ref<any[]>([])
const selected = ref<any>(null)
const versions = ref<any[]>([])
const labels = ref<Record<string, any>>({})

const toggleLocale = () => {
  const next = locale.value === 'zh-CN' ? 'en' : 'zh-CN'
  locale.value = next
  localStorage.setItem('woca.skillsvc.locale', next)
}

async function refresh() {
  const res = await fetch('/api/skills')
  assets.value = await res.json() || []
}

async function select(a: any) {
  selected.value = a
  const [vs, ls] = await Promise.all([
    fetch(`/api/skills/${a.id}/versions`).then(r => r.json()),
    fetch(`/api/skills/${a.id}/labels`).then(r => r.json()),
  ])
  versions.value = vs || []
  labels.value = ls || {}
}

onMounted(refresh)
defineExpose({ refresh, select })
</script>

<template>
  <div class="console">
    <header class="header">
      <div>
        <h1>{{ $t('title') }}</h1>
        <p class="subtitle">{{ $t('subtitle') }}</p>
      </div>
      <button class="locale-toggle" @click="toggleLocale">
        {{ locale === 'zh-CN' ? 'EN' : '中文' }}
      </button>
    </header>
    <main class="grid">
      <AssetList :assets="assets" :selected="selected" @select="select" @refresh="refresh" />
      <template v-if="selected">
        <VersionPanel :asset="selected" :versions="versions" @refresh="select(selected)" />
        <LabelPanel :asset="selected" :versions="versions" :labels="labels" @refresh="select(selected)" />
      </template>
    </main>
  </div>
</template>

<style>
/* Aligned with the console family's light monochrome tokens (the shared
 * styles.css carries the woca-mono theme); layout stays the 3-panel grid. */
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: system-ui, -apple-system, sans-serif; background: var(--bg-page, #f5f6f7); color: #1f2933; }
.console { max-width: 1280px; margin: 0 auto; padding: 24px; }
.header { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 20px; }
h1 { font-size: 18px; font-weight: 600; }
.subtitle { color: #616e7c; font-size: 12px; margin-top: 4px; }
.locale-toggle { padding: 4px 10px; border-radius: 6px; border: 1px solid #cbd2d9; background: none; color: #1f2933; cursor: pointer; font: inherit; }
.grid { display: grid; grid-template-columns: 300px 1fr 1fr; gap: 16px; align-items: start; }
</style>
