<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ assets: any[]; selected: any }>()
const emit = defineEmits<{ select: [a: any]; refresh: [] }>()
const { t } = useI18n()
const showCreate = ref(false)
const newName = ref('')
const newDesc = ref('')

async function create() {
  if (!newName.value.trim()) return
  await fetch('/api/skills', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: newName.value, description: newDesc.value }),
  })
  newName.value = ''
  newDesc.value = ''
  showCreate.value = false
  emit('refresh')
}

async function del(a: any) {
  if (!confirm(t('assets.confirmDelete'))) return
  await fetch(`/api/skills/${a.id}`, { method: 'DELETE' })
  emit('refresh')
}
</script>

<template>
  <section class="panel">
    <h2>{{ t('assets.list') }}</h2>
    <button class="btn primary" @click="showCreate = !showCreate">{{ t('assets.create') }}</button>
    <form v-if="showCreate" @submit.prevent="create" class="create-form">
      <input v-model="newName" :placeholder="t('assets.name')" required />
      <input v-model="newDesc" :placeholder="t('assets.description')" />
      <button type="submit" class="btn primary">{{ t('actions.save') }}</button>
    </form>
    <ul v-if="assets.length" class="asset-list">
      <li v-for="a in assets" :key="a.id" :class="{ active: selected?.id === a.id }" @click="emit('select', a)">
        <div class="asset-name">{{ a.name }}</div>
        <div class="asset-desc">{{ a.description }}</div>
        <button class="btn danger small" @click.stop="del(a)">{{ t('assets.delete') }}</button>
      </li>
    </ul>
    <p v-else class="empty">{{ t('assets.empty') }}</p>
  </section>
</template>

<style scoped>
.panel { background: #fff; border: 1px solid #e4e7eb; border-radius: 10px; padding: 16px; }
h2 { font-size: 15px; margin-bottom: 10px; color: #1f2933; }
.btn { padding: 6px 14px; border-radius: 8px; border: none; cursor: pointer; font-size: 13px; }
.btn.primary { background: #111; color: #fff; }
.btn.danger { background: none; color: #c0392b; border: 1px solid #c0392b; }
.btn.small { padding: 2px 8px; font-size: 11px; }
.create-form { display: flex; flex-direction: column; gap: 8px; margin: 10px 0; }
input { padding: 8px 10px; border-radius: 8px; border: 1px solid #cbd2d9; background: transparent; color: inherit; font-size: 13px; }
.asset-list { list-style: none; }
.asset-list li { padding: 10px; border-radius: 8px; cursor: pointer; margin-bottom: 4px; border: 1px solid transparent; }
.asset-list li:hover { background: #eceef0; }
.asset-list li.active { border-color: #cbd2d9; background: #e4e7eb; }
.asset-name { font-weight: 600; font-size: 14px; }
.asset-desc { color: #616e7c; font-size: 12px; margin-top: 2px; }
.empty { color: #616e7c; font-size: 13px; padding: 16px 0; text-align: center; }
</style>
