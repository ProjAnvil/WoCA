<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectOps } from '../inject'

// Governance audit (R5): the service-plane trail — key issuance/revocation,
// bridge enrollment/revocation, enrollment tokens and approvals.
const { t } = useI18n()
const { api } = injectOps()

interface AuditRow {
  at: number
  event: string
  bridge_id?: string
  detail?: string
}

const rows = ref<AuditRow[]>([])
const total = ref(0)
const eventNames = ref<string[]>([])
const bridgeFilter = ref('')
const eventFilter = ref('')
const fromFilter = ref('')
const toFilter = ref('')
const PAGE = 50
const offset = ref(0)
const loading = ref(true)
const error = ref('')

async function load() {
  error.value = ''
  try {
    const params = new URLSearchParams({ limit: String(PAGE), offset: String(offset.value) })
    if (bridgeFilter.value) params.set('bridge', bridgeFilter.value)
    if (eventFilter.value) params.set('event', eventFilter.value)
    if (fromFilter.value) params.set('from', String(Date.parse(fromFilter.value) || 0))
    if (toFilter.value) params.set('to', String(Date.parse(toFilter.value + 'T23:59:59') || 0))
    const resp = await api(`/api/gw/audit?${params}`)
    if (!resp.ok) throw new Error(`${resp.status}`)
    const doc = await resp.json()
    rows.value = doc.items ?? []
    total.value = doc.total ?? 0
  } catch (e) {
    error.value = String(e)
  } finally {
    loading.value = false
  }
}

async function loadVocab() {
  try {
    const e = await api('/api/gw/audit/events')
    if (e.ok) eventNames.value = (await e.json()) ?? []
  } catch {
    /* filters stay free-text-empty */
  }
}

let bridgeTimer: ReturnType<typeof setTimeout> | undefined
function onBridgeInput() {
  clearTimeout(bridgeTimer)
  bridgeTimer = setTimeout(apply, 250)
}

function apply() {
  offset.value = 0
  void load()
}

const page = computed(() => Math.floor(offset.value / PAGE) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE)))
function prevPage() {
  offset.value = Math.max(0, offset.value - PAGE)
  void load()
}
function nextPage() {
  if (offset.value + PAGE < total.value) {
    offset.value += PAGE
    void load()
  }
}

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}

onMounted(() => {
  void load()
  void loadVocab()
})
</script>

<template>
  <section class="audit-card">
    <div class="audit-head">
      <div>
        <div class="audit-title">{{ t('audit.title') }}</div>
        <p class="audit-hint">{{ t('audit.hint') }}</p>
      </div>
      <button class="btn" @click="load">{{ t('audit.refresh') }}</button>
    </div>
    <p v-if="error" class="audit-error">{{ error }}</p>

    <div class="audit-filters">
      <input
        v-model="bridgeFilter"
        class="audit-bridge-input"
        :placeholder="t('audit.bridgePlaceholder')"
        @input="onBridgeInput"
      />
      <select v-model="eventFilter" @change="apply">
        <option value="">{{ t('audit.allEvents') }}</option>
        <option v-for="e in eventNames" :key="e" :value="e">{{ e }}</option>
      </select>
      <input v-model="fromFilter" type="date" :title="t('audit.from')" @change="apply" />
      <span>–</span>
      <input v-model="toFilter" type="date" :title="t('audit.to')" @change="apply" />
    </div>

    <table v-if="rows.length" class="audit-table">
      <thead>
        <tr>
          <th>{{ t('audit.when') }}</th>
          <th>{{ t('audit.event') }}</th>
          <th>{{ t('audit.bridge') }}</th>
          <th>{{ t('audit.detail') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="(r, i) in rows" :key="i">
          <td>{{ fmtTime(r.at) }}</td>
          <td><code>{{ r.event }}</code></td>
          <td><code v-if="r.bridge_id">{{ r.bridge_id }}</code></td>
          <td class="audit-detail">{{ r.detail }}</td>
        </tr>
      </tbody>
    </table>
    <p v-else-if="!loading" class="audit-hint">{{ t('audit.empty') }}</p>

    <div v-if="total > PAGE || page > 1" class="audit-pager">
      <button class="btn" :disabled="offset === 0" @click="prevPage">‹</button>
      <span>{{ page }} / {{ pages }}</span>
      <button class="btn" :disabled="offset + PAGE >= total" @click="nextPage">›</button>
    </div>
  </section>
</template>

<style scoped>
.audit-card {
  border: 1px solid var(--border, #e4e7eb);
  border-radius: 10px;
  padding: 12px 14px;
}
.audit-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  margin-bottom: 8px;
}
.audit-title {
  font-weight: 600;
}
.audit-hint {
  margin: 0;
  font-size: 12.5px;
  opacity: 0.75;
}
.audit-error {
  color: #e5534b;
  font-size: 12.5px;
  margin: 0 0 8px;
}
.audit-filters {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
  font-size: 12.5px;
}
.audit-bridge-input {
  min-width: 160px;
}
.audit-filters select, .audit-filters input {
  padding: 6px 8px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 6px;
  background: transparent;
  color: inherit;
  font: inherit;
  font-size: 12.5px;
}
.audit-pager {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 10px;
  font-size: 12.5px;
}
.audit-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
}
.audit-table th {
  text-align: left;
  opacity: 0.6;
  font-weight: 500;
  padding: 4px 8px 4px 0;
}
.audit-table td {
  padding: 5px 8px 5px 0;
  border-top: 1px solid var(--border, #e4e7eb);
}
.audit-detail {
  font-family: ui-monospace, monospace;
  overflow-wrap: anywhere;
  max-width: 260px;
}
.btn {
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 6px;
  background: none;
  padding: 5px 10px;
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
}
</style>
