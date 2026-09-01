<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectOps } from '../inject'

// 登录管理 (product design v1.8 §2.1): the core-issued login-state
// inventory — every active session with per-session revocation. Killing
// one session kicks that single login (the key stays valid and the user
// can log in again); it is the precise instrument next to key
// revocation, which kills everything.
const { t } = useI18n()
const { api } = injectOps()

interface CoreSession {
  token_hash: string
  key_prefix: string
  remark: string
  role: string
  tenant_id: string
  tenant_name: string
  created_at: number
  last_seen: number
  expires_at: number
  revoked_at?: number
}

const sessions = ref<CoreSession[]>([])
const total = ref(0)
const query = ref('')
const orderBy = ref('last_used_desc')
const PAGE = 20
const offset = ref(0)
const loading = ref(true)
const error = ref('')
const killing = ref('')

async function load() {
  error.value = ''
  try {
    const resp = await api(`/api/tenant/sessions?q=${encodeURIComponent(query.value.trim())}&order=${orderBy.value}&limit=${PAGE}&offset=${offset.value}`)
    if (resp.status === 401) return
    if (!resp.ok) throw new Error(`${resp.status}`)
    const doc = await resp.json()
    sessions.value = doc.items ?? []
    total.value = doc.total ?? 0
  } catch (e) {
    error.value = String(e)
  } finally {
    loading.value = false
  }
}

let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    offset.value = 0
    void load()
  }, 250)
}

const page = computed(() => Math.floor(offset.value / PAGE) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE)))
function prevPage() {
  offset.value = Math.max(0, offset.value - PAGE)
  void load()
}
function sortBy(field: 'issued' | 'last_used' | 'expires') {
  orderBy.value = orderBy.value === field ? `${field}_desc` : field
  offset.value = 0
  void load()
}

function nextPage() {
  if (offset.value + PAGE < total.value) {
    offset.value += PAGE
    void load()
  }
}

async function killSession(sess: CoreSession) {
  if (killing.value) return
  if (!confirm(t('sessions.killConfirm', { remark: sess.remark }))) return
  killing.value = sess.token_hash
  error.value = ''
  try {
    const resp = await api(`/api/tenant/sessions/${encodeURIComponent(sess.token_hash)}`, { method: 'DELETE' })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    killing.value = ''
  }
}

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}

onMounted(load)
</script>

<template>
  <section class="sessions-card">
    <div class="sessions-title">{{ t('sessions.title') }}</div>
    <p class="sessions-hint">{{ t('sessions.hint') }}</p>
    <p v-if="error" class="sessions-error">{{ error }}</p>

    <input
      v-model="query"
      class="sessions-search"
      :placeholder="t('common.searchUserTenant')"
      @input="onSearch"
    />

    <table v-if="sessions.length" class="sessions-table">
      <thead>
        <tr>
          <th>{{ t('sessions.colRemark') }}</th>
          <th>{{ t('sessions.colTenant') }}</th>
          <th>{{ t('sessions.colRole') }}</th>
          <th>{{ t('sessions.colKey') }}</th>
          <th class="sortable" @click="sortBy('issued')">{{ t('sessions.colCreated') }} {{ orderBy.startsWith('issued') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}</th>
          <th class="sortable" @click="sortBy('last_used')">{{ t('sessions.colLastSeen') }} {{ orderBy.startsWith('last_used') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}</th>
          <th class="sortable" @click="sortBy('expires')">{{ t('sessions.colExpires') }} {{ orderBy.startsWith('expires') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}</th>
          <th>{{ t('sessions.colState') }}</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="sess in sessions" :key="sess.token_hash" :class="{ revoked: sess.revoked_at }">
          <td>{{ sess.remark }}</td>
          <td>{{ sess.tenant_name || sess.tenant_id }}</td>
          <td><code>{{ sess.role }}</code></td>
          <td><code class="sessions-key" :title="sess.key_prefix">{{ sess.key_prefix }}</code></td>
          <td>{{ fmtTime(sess.last_seen) }}</td>
          <td>{{ fmtTime(sess.expires_at) }}</td>
          <td :class="sess.revoked_at ? 'sessions-off' : 'sessions-live'">
            {{ sess.revoked_at ? t('sessions.stateRevoked') : '● ' + t('sessions.stateActive') }}
          </td>
          <td>
            <button v-if="!sess.revoked_at" class="btn btn-danger" :disabled="killing === sess.token_hash" @click="killSession(sess)">
              {{ t('sessions.kill') }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else-if="!loading" class="sessions-hint">{{ t('sessions.empty') }}</p>

    <div v-if="total > PAGE || page > 1" class="sessions-pager">
      <button class="btn" :disabled="offset === 0" @click="prevPage">‹</button>
      <span>{{ page }} / {{ pages }}</span>
      <button class="btn" :disabled="offset + PAGE >= total" @click="nextPage">›</button>
    </div>
  </section>
</template>

<style scoped>
.sessions-card {
  border: 1px solid #e4e7eb;
  border-radius: 10px;
  padding: 12px 14px;
}
.sessions-title {
  font-weight: 600;
  margin-bottom: 6px;
}
.sessions-hint {
  margin: 0 0 10px;
  font-size: 12.5px;
  opacity: 0.75;
}
.sessions-error {
  color: #e5534b;
  font-size: 12.5px;
  margin: 0 0 8px;
}
.sortable {
  cursor: pointer;
  user-select: none;
}
.sortable:hover {
  opacity: 1;
}
.sessions-search {
  width: 100%;
  padding: 7px 10px;
  margin-bottom: 10px;
  border: 1px solid #cbd2d9;
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
}
.sessions-pager {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 10px;
  font-size: 12.5px;
}
.sessions-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
}
.sessions-table th {
  text-align: left;
  opacity: 0.6;
  font-weight: 500;
  padding: 4px 8px 4px 0;
}
.sessions-table td {
  padding: 5px 8px 5px 0;
  border-top: 1px solid #e4e7eb;
}
.sessions-table tr.revoked td {
  opacity: 0.55;
}
.sessions-live {
  color: #047857;
}
.sessions-off {
  color: #e5534b;
}
.sessions-key {
  font-family: ui-monospace, monospace;
  font-size: 11px;
  opacity: 0.8;
}
.btn {
  border: 1px solid #cbd2d9;
  border-radius: 6px;
  background: none;
  padding: 5px 10px;
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
}
.btn-danger {
  border-color: #c0392b;
  color: #c0392b;
}
</style>
