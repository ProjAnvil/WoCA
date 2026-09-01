<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ asset: any; versions: any[] }>()
const emit = defineEmits<{ refresh: [] }>()
const { t } = useI18n()
const body = ref('')
const publishing = ref(false)

async function publish() {
  if (!body.value.trim()) return
  publishing.value = true
  await fetch(`/api/skills/${props.asset.id}/versions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ body: body.value }),
  })
  body.value = ''
  publishing.value = false
  emit('refresh')
}

function fmt(ts: number) {
  return new Date(ts).toLocaleString()
}
</script>

<template>
  <section class="panel">
    <h2>{{ t('versions.title') }} — {{ asset.name }}</h2>
    <form @submit.prevent="publish" class="publish-form">
      <textarea v-model="body" :placeholder="t('versions.bodyPlaceholder')" rows="4" />
      <button type="submit" class="btn primary" :disabled="publishing || !body.trim()">
        {{ publishing ? '…' : t('versions.publish') }}
      </button>
    </form>
    <ul v-if="versions.length" class="version-list">
      <li v-for="v in [...versions].reverse()" :key="v.version">
        <span class="ver">v{{ v.version }}</span>
        <span class="date">{{ fmt(v.created_at) }}</span>
        <code class="hash">{{ v.checksum.slice(0, 12) }}…</code>
      </li>
    </ul>
    <p v-else class="empty">{{ t('versions.empty') }}</p>
  </section>
</template>

<style scoped>
.panel { background: #fff; border: 1px solid #e4e7eb; border-radius: 10px; padding: 16px; }
h2 { font-size: 15px; margin-bottom: 10px; color: #1f2933; }
.btn { padding: 6px 14px; border-radius: 8px; border: none; cursor: pointer; font-size: 13px; }
.btn.primary { background: #111; color: #fff; }
.btn:disabled { opacity: 0.5; }
.publish-form { display: flex; flex-direction: column; gap: 8px; margin-bottom: 14px; }
textarea { padding: 10px; border-radius: 8px; border: 1px solid #cbd2d9; background: transparent; color: inherit; font-size: 13px; font-family: ui-monospace, monospace; resize: vertical; }
.version-list { list-style: none; }
.version-list li { display: flex; align-items: center; gap: 10px; padding: 6px 0; border-bottom: 1px solid #e4e7eb; }
.ver { font-weight: 700; color: #1f2933; min-width: 40px; }
.date { color: #616e7c; font-size: 12px; flex: 1; }
.hash { color: #a8b0b9; font-size: 11px; }
.empty { color: #616e7c; font-size: 13px; padding: 12px 0; text-align: center; }
</style>
