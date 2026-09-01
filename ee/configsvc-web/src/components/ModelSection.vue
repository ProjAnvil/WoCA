<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { save, saving } = injectCfg()
const { api } = injectCfg()

interface Preset {
  id: string
  name: string
  provider: string
  model: string
  max_tokens: number
  temperature: number
  base_url: string
  key_set: boolean
  updated_at: number
}

const presets = ref<Preset[]>([])
const editing = ref<Preset | null>(null)
const newApiKey = ref('')
const err = ref('')
const flash = ref('')

const providers = ['anthropic', 'openai', 'glm', 'deepseek'] as const

async function refresh() {
  err.value = ''
  try {
    const resp = await api('/api/models')
    if (!resp.ok) throw new Error(`${resp.status}`)
    presets.value = (await resp.json()) ?? []
  } catch (e) {
    err.value = String(e)
  }
}
refresh()

function blank(): Preset {
  return { id: '', name: '', provider: 'anthropic', model: '', max_tokens: 4096, temperature: 0, base_url: '', key_set: false, updated_at: 0 }
}

async function persist() {
  if (!editing.value) return
  err.value = ''
  flash.value = ''
  try {
    const body: Record<string, unknown> = { ...editing.value }
    if (newApiKey.value) body['api_key'] = newApiKey.value
    const resp = await api('/api/models', { method: 'POST', body: JSON.stringify(body) })
    const doc = await resp.json().catch(() => ({}))
    if (!resp.ok) throw new Error(doc.error || `${resp.status}`)
    editing.value = doc as Preset
    newApiKey.value = ''
    flash.value = t('model.saved')
    await refresh()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  }
}

async function remove(id: string) {
  err.value = ''
  try {
    const resp = await api(`/api/models/${id}`, { method: 'DELETE' })
    if (!resp.ok) throw new Error(`${resp.status}`)
    if (editing.value?.id === id) editing.value = null
    await refresh()
  } catch (e) {
    err.value = String(e)
  }
}
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('nav.model') }}</div>
    <p class="hint">{{ t('model.presetIntro') }}</p>

    <div class="preset-list">
      <div v-for="p in presets" :key="p.id" class="preset-row" :class="{ active: editing?.id === p.id }" @click="editing = p">
        <div class="preset-main">
          <span class="preset-name">{{ p.name }}</span>
          <span class="preset-sub">{{ p.provider }} · {{ p.model }}</span>
        </div>
        <button class="ghost danger" @click.stop="remove(p.id)">{{ t('model.delete') }}</button>
      </div>
      <div v-if="!presets.length" class="hint">{{ t('model.empty') }}</div>
    </div>
    <button class="ghost" style="margin: 6px 0 14px" @click="editing = blank(); newApiKey = ''">{{ t('model.newPreset') }}</button>

    <div v-if="editing" class="editor">
      <div class="row">
        <label>{{ t('model.presetName') }}</label>
        <input class="field" v-model="editing.name" />
      </div>
      <div class="row">
        <label>{{ t('model.provider') }}</label>
        <select class="field" v-model="editing.provider">
          <option v-for="p in providers" :key="p" :value="p">{{ p }}</option>
        </select>
      </div>
      <div class="row">
        <label>{{ t('model.modelName') }}</label>
        <input class="field" v-model="editing.model" />
      </div>
      <div class="row">
        <label>{{ t('model.maxTokens') }}</label>
        <input class="field" type="number" v-model.number="editing.max_tokens" />
      </div>
      <div class="row">
        <label>{{ t('model.temperature') }}</label>
        <input class="field" type="number" step="0.1" v-model.number="editing.temperature" />
      </div>
      <div class="row">
        <label>{{ t('model.baseURL') }}</label>
        <input class="field" v-model="editing.base_url" :placeholder="t('model.baseURLPlaceholder')" />
      </div>
      <div class="row">
        <label>{{ t('model.apiKey') }}</label>
        <input class="field" type="password" v-model="newApiKey" :placeholder="editing.key_set ? t('storage.pwSet') : t('storage.pwPlaceholder')" />
      </div>
      <div v-if="flash" class="ok">{{ flash }}</div>
      <div v-if="err" class="err">{{ err }}</div>
      <button class="primary" :disabled="saving" style="margin-top: 8px" @click="persist">{{ t('common.save') }}</button>
    </div>
  </section>
</template>

<style scoped>
.preset-list { display: flex; flex-direction: column; gap: 4px; margin-bottom: 4px; }
.preset-row { display: flex; justify-content: space-between; align-items: center; gap: 10px; padding: 7px 10px; border: 1px solid #e4e7eb; border-radius: 6px; cursor: pointer; }
.preset-row.active { background: #e4e7eb; border-color: #cbd2d9; }
.preset-main { display: flex; flex-direction: column; gap: 1px; }
.preset-name { font-weight: 500; font-size: 13px; }
.preset-sub { color: #616e7c; font-size: 11px; }
.danger { color: #c0392b; }
.editor { border-top: 1px solid #e4e7eb; padding-top: 12px; margin-top: 4px; }
</style>
