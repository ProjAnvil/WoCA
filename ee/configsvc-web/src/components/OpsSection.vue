<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { s, save, api } = injectCfg()

interface ServiceAck {
  service_id: string
  role: string
  generation: string
  addr: string
  updated_at: number
  live: boolean
  config_lagging: boolean
}
const services = ref<ServiceAck[]>([])
const generation = ref('')
const tenantGeneration = ref('')
const statusError = ref('')

async function refreshStatus() {
  statusError.value = ''
  try {
    const resp = await api('/api/config/status')
    if (!resp.ok) throw new Error(`${resp.status}`)
    const doc = await resp.json()
    services.value = doc.services ?? []
    generation.value = doc.generation ?? ''
    tenantGeneration.value = doc.tenant_generation ?? ''
  } catch (e) {
    statusError.value = String(e)
  }
}
onMounted(refreshStatus)

const includeSecrets = ref(false)
const exportDoc = ref('')
const importDoc = ref('')
const applied = ref('')
const opError = ref('')

async function doExport() {
  opError.value = ''
  applied.value = ''
  try {
    const resp = await api('/api/settings/export' + (includeSecrets.value ? '?include_secrets=true' : ''))
    if (!resp.ok) throw new Error(`${resp.status}`)
    exportDoc.value = JSON.stringify(await resp.json(), null, 2)
  } catch (e) {
    opError.value = String(e)
  }
}

async function doImport() {
  opError.value = ''
  applied.value = ''
  try {
    const resp = await api('/api/settings/import', { method: 'POST', body: importDoc.value })
    const doc = await resp.json().catch(() => ({}))
    if (!resp.ok) throw new Error(doc.error || `${resp.status}`)
    applied.value = `${t('ops.imported')}: ${doc.values ?? 0}`
    importDoc.value = ''
    window.location.reload()
  } catch (e) {
    opError.value = e instanceof Error ? e.message : String(e)
  }
}
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('ops.title') }}</div>
    <p class="hint">{{ t('ops.intro') }}</p>

    <div class="row">
      <label>{{ t('ops.export') }}</label>
      <label class="inline"><input type="checkbox" v-model="includeSecrets" /> {{ t('ops.exportSecrets') }}</label>
      <button class="ghost" @click="doExport">{{ t('ops.export') }}</button>
    </div>
    <textarea v-if="exportDoc" v-model="exportDoc" class="field" rows="10" readonly></textarea>

    <div class="row">
      <label>{{ t('ops.import') }}</label>
    </div>
    <textarea v-model="importDoc" class="field" rows="8" :placeholder="t('ops.importPlaceholder')"></textarea>
    <div v-if="applied" class="ok">{{ applied }}</div>
    <div v-if="opError" class="err">{{ opError }}</div>
    <button class="primary" style="margin-top: 8px" :disabled="!importDoc" @click="doImport">{{ t('ops.apply') }}</button>

    <div class="row" style="margin-top: 20px; align-items: baseline">
      <label style="font-weight: 600">{{ t('ops.status') }}</label>
      <button class="ghost" @click="refreshStatus">{{ t('ops.refresh') }}</button>
    </div>
    <div v-if="generation" class="hint">
      {{ t('ops.generation') }}: {{ generation }} · {{ t('ops.tenantGeneration') }}: {{ tenantGeneration }}
    </div>
    <div v-if="statusError" class="err">{{ statusError }}</div>
    <table v-if="services.length" class="acks">
      <thead>
        <tr><th>{{ t('ops.role') }}</th><th>ID</th><th>{{ t('ops.gen') }}</th><th>{{ t('ops.state') }}</th></tr>
      </thead>
      <tbody>
        <tr v-for="sv in [...services].sort((a, b) => a.role.localeCompare(b.role) || a.service_id.localeCompare(b.service_id))" :key="sv.service_id">
          <td>{{ sv.role }}</td>
          <td>{{ sv.service_id }}</td>
          <td class="mono">{{ sv.generation }}</td>
          <td>
            <span :class="sv.live ? 'ok' : 'err'">{{ sv.live ? t('ops.live') : t('ops.stale') }}</span>
            <span v-if="sv.config_lagging" class="err"> · {{ t('ops.lagging') }}</span>
          </td>
        </tr>
      </tbody>
    </table>

    <div class="row" style="margin-top: 16px">
      <label>{{ t('ops.debugLog') }}</label>
      <input type="checkbox" :checked="s?.debug_log" @change="save({ debug_log: ($event.target as HTMLInputElement).checked })" />
    </div>
  </section>
</template>
