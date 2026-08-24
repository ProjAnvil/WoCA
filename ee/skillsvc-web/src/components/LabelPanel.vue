<script setup lang="ts">
import { useI18n } from 'vue-i18n'

const props = defineProps<{ asset: any; versions: any[]; labels: Record<string, any> }>()
const emit = defineEmits<{ refresh: [] }>()
const { t } = useI18n()

async function setLabel(label: string, version: number) {
  await fetch(`/api/skills/${props.asset.id}/labels/${label}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ version }),
  })
  emit('refresh')
}
</script>

<template>
  <section class="panel">
    <h2>{{ t('labels.title') }} — {{ asset.name }}</h2>
    <div v-for="label in ['production', 'canary']" :key="label" class="label-row">
      <div class="label-info">
        <span class="label-name" :class="label">{{ label }}</span>
        <span v-if="labels[label]" class="label-current">
          {{ t('labels.current') }}: v{{ labels[label].version }}
        </span>
        <span v-else class="label-none">{{ t('labels.none') }}</span>
      </div>
      <select @change="setLabel(label, +($event.target as HTMLSelectElement).value)" :value="labels[label]?.version ?? ''">
        <option value="" disabled>—</option>
        <option v-for="v in [...versions].reverse()" :key="v.version" :value="v.version">v{{ v.version }}</option>
      </select>
    </div>
  </section>
</template>

<style scoped>
.panel { background: #131826; border: 1px solid #2a3048; border-radius: 12px; padding: 16px; }
h2 { font-size: 15px; margin-bottom: 10px; color: #c6cde6; }
.label-row { display: flex; justify-content: space-between; align-items: center; padding: 10px 0; border-bottom: 1px solid #1a2036; }
.label-name { font-weight: 700; font-size: 13px; }
.label-name.production { color: #4ade80; }
.label-name.canary { color: #fcd34d; }
.label-current { color: #97a0bd; font-size: 12px; margin-left: 10px; }
.label-none { color: #556; font-size: 12px; margin-left: 10px; }
select { padding: 6px 10px; border-radius: 8px; border: 1px solid #3a4160; background: #0d1220; color: #e8eaf2; font-size: 13px; }
</style>
