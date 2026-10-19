<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
import type { SandboxBackend, VerifyResult } from '../types'
const { t } = useI18n()
const { api } = injectCfg()

const sandboxTypes = ['contract-http', 'e2b', 'daytona', 'modal', 'microsandbox', 'cfbridge', 'sidecar'] as const

const backends = ref<SandboxBackend[]>([])
const editing = ref<SandboxBackend | null>(null)
const newApiKey = ref('')
const err = ref('')
const flash = ref('')
const verifying = ref(false)
const verifyResult = ref<VerifyResult | null>(null)

async function refresh() {
  err.value = ''
  try {
    const resp = await api('/api/sandboxes')
    if (!resp.ok) throw new Error(`${resp.status}`)
    backends.value = (await resp.json()) ?? []
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  }
}
refresh()

function blank(): SandboxBackend {
  return { id: '', name: '', type: 'contract-http', endpoint: '', timeout_ms: 120000, enabled: false, key_set: false }
}

const formValid = () =>
  !!editing.value && editing.value.name.trim() !== '' && editing.value.endpoint.trim() !== ''

async function persist() {
  if (!editing.value || !formValid()) return
  err.value = ''
  flash.value = ''
  verifyResult.value = null
  try {
    const body: Record<string, unknown> = { ...editing.value }
    if (newApiKey.value) body['api_key'] = newApiKey.value
    const isNew = !editing.value.id
    const resp = await api(isNew ? '/api/sandboxes' : `/api/sandboxes/${editing.value.id}`, {
      method: isNew ? 'POST' : 'PUT',
      body: JSON.stringify(body),
    })
    const doc = await resp.json().catch(() => ({}))
    if (!resp.ok) throw new Error(doc.error || `${resp.status}`)
    editing.value = null
    newApiKey.value = ''
    flash.value = t('common.saved')
    await refresh()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  }
}

async function remove(id: string) {
  err.value = ''
  try {
    const resp = await api(`/api/sandboxes/${id}`, { method: 'DELETE' })
    const doc = await resp.json().catch(() => ({}))
    if (!resp.ok) throw new Error(doc.error || `${resp.status}`)
    if (editing.value?.id === id) editing.value = null
    await refresh()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  }
}

async function toggle(b: SandboxBackend, enabled: boolean) {
  err.value = ''
  try {
    const resp = await api(`/api/sandboxes/${b.id}`, { method: 'PUT', body: JSON.stringify({ enabled }) })
    const doc = await resp.json().catch(() => ({}))
    if (!resp.ok) throw new Error(doc.error || `${resp.status}`)
    await refresh()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  }
}

async function verify(id: string) {
  err.value = ''
  verifyResult.value = null
  verifying.value = true
  try {
    const resp = await api(`/api/sandboxes/${id}/verify`, { method: 'POST' })
    if (!resp.ok) throw new Error(`${resp.status}`)
    verifyResult.value = (await resp.json()) as VerifyResult
    await refresh()
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    verifying.value = false
  }
}

function probeTime(ms?: number): string {
  if (!ms) return ''
  return new Date(ms).toLocaleString()
}
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('nav.sandbox') }}</div>
    <p class="hint">{{ t('sandbox.intro') }}</p>

    <div class="backend-list">
      <div v-for="b in backends" :key="b.id" class="backend-row" :class="{ active: editing?.id === b.id }" @click="editing = b; newApiKey = ''; verifyResult = null">
        <div class="backend-main">
          <span class="backend-name">{{ b.name }} <span v-if="b.enabled" class="badge on">{{ t('sandbox.enabled') }}</span></span>
          <span class="backend-sub">{{ b.type }} · {{ b.endpoint }}</span>
          <span class="backend-sub probe">
            <span v-if="b.last_probe_at" :class="b.last_probe_ok ? 'ok' : 'err'">
              {{ b.last_probe_ok ? t('sandbox.probeOk') : t('sandbox.probeFail') }} · {{ probeTime(b.last_probe_at) }}
            </span>
            <span v-else class="muted">{{ t('sandbox.probeNever') }}</span>
            <span v-if="b.last_probe_error" class="err"> · {{ b.last_probe_error }}</span>
          </span>
        </div>
        <div class="backend-actions" @click.stop>
          <label class="inline"><input type="checkbox" :checked="b.enabled" @change="toggle(b, ($event.target as HTMLInputElement).checked)" /> {{ t('sandbox.enable') }}</label>
          <button class="ghost" :disabled="verifying" @click="verify(b.id)">{{ verifying ? t('sandbox.verifying') : t('sandbox.test') }}</button>
          <button class="ghost danger" :disabled="b.enabled" :title="b.enabled ? t('sandbox.deleteDisabled') : ''" @click="remove(b.id)">{{ t('model.delete') }}</button>
        </div>
      </div>
      <div v-if="!backends.length" class="hint">{{ t('sandbox.empty') }}</div>
    </div>
    <button class="ghost" style="margin: 6px 0 14px" @click="editing = blank(); newApiKey = ''; verifyResult = null">{{ t('sandbox.newBackend') }}</button>

    <div v-if="editing" class="editor">
      <div class="row">
        <label>{{ t('sandbox.name') }}</label>
        <input class="field" v-model="editing.name" />
      </div>
      <div class="row">
        <label>{{ t('sandbox.type') }}</label>
        <select class="field" v-model="editing.type">
          <option v-for="ty in sandboxTypes" :key="ty" :value="ty">{{ ty }}</option>
        </select>
      </div>
      <div class="row">
        <label>{{ t('sandbox.endpoint') }}</label>
        <input class="field" v-model="editing.endpoint" placeholder="http://sandbox:8080" />
      </div>
      <div class="row">
        <label>{{ t('sandbox.timeout') }}</label>
        <input class="field" type="number" v-model.number="editing.timeout_ms" />
      </div>
      <div class="row">
        <label>{{ t('sandbox.apiKey') }}</label>
        <input class="field" type="password" v-model="newApiKey" :placeholder="editing.key_set ? t('sandbox.keySet') : t('storage.pwPlaceholder')" />
      </div>
      <div v-if="flash" class="ok">{{ flash }}</div>
      <div v-if="err" class="err">{{ err }}</div>
      <button class="primary" :disabled="!formValid()" style="margin-top: 8px" @click="persist">{{ t('common.save') }}</button>

      <div v-if="verifyResult" class="verify">
        <div :class="verifyResult.ok ? 'ok' : 'err'">
          {{ verifyResult.ok ? t('sandbox.verifyOk') : t('sandbox.verifyFail') }} · {{ verifyResult.total_latency_ms }}ms
        </div>
        <details>
          <summary class="hint">{{ t('sandbox.verifySteps') }}</summary>
          <table class="steps">
            <tbody>
              <tr v-for="st in verifyResult.steps" :key="st.name">
                <td class="mono">{{ st.name }}</td>
                <td><span :class="st.ok ? 'ok' : 'err'">{{ st.ok ? '✓' : '✗' }}</span> {{ st.latency_ms }}ms</td>
                <td v-if="st.error" class="err">{{ st.error }}</td>
              </tr>
            </tbody>
          </table>
        </details>
      </div>
    </div>
  </section>
</template>

<style scoped>
.backend-list { display: flex; flex-direction: column; gap: 4px; margin-bottom: 4px; }
.backend-row { display: flex; justify-content: space-between; align-items: center; gap: 10px; padding: 7px 10px; border: 1px solid #e4e7eb; border-radius: 6px; cursor: pointer; }
.backend-row.active { background: #e4e7eb; border-color: #cbd2d9; }
.backend-main { display: flex; flex-direction: column; gap: 1px; }
.backend-name { font-weight: 500; font-size: 13px; }
.backend-sub { color: #616e7c; font-size: 11px; }
.backend-actions { display: flex; align-items: center; gap: 8px; }
.badge { font-size: 10px; border-radius: 4px; padding: 1px 6px; }
.badge.on { background: #d1fae5; color: #047857; }
.danger { color: #c0392b; }
.danger:disabled { color: #b7bdc4; cursor: not-allowed; }
.muted { color: #9aa5b1; }
.editor { border-top: 1px solid #e4e7eb; padding-top: 12px; margin-top: 4px; }
.verify { border-top: 1px dashed #e4e7eb; margin-top: 12px; padding-top: 8px; }
.steps { margin-top: 6px; }
.steps td { padding: 2px 10px 2px 0; font-size: 12px; }
</style>
