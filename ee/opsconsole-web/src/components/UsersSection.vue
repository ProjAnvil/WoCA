<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectOps } from '../inject'

// Tenants & API keys (product design v1.8 §6.7/§2.1): issue with a
// username remark and role — the woca_ key shows exactly once and
// travels to the user out of band; revocation kills the key AND its
// core-issued sessions on the spot. "Last used" is authoritative
// (logins and validations happen at the core). The live-session
// inventory lives in its own section (登录管理, SessionsSection).
const { t } = useI18n()
const { api } = injectOps()

interface TenantKey {
  prefix: string
  remark: string
  role: string
  tenant_id: string
  tenant_name: string
  issued_at: number
  revoked_at?: number
  last_used?: number
}

interface Tenant {
  id: string
  name: string
  created_at: number
}

interface TenantStat {
  tenant_id: string
  tenant_name: string
  keys_active: number
  keys_revoked: number
  sessions: number
  last_used?: number
}

const users = ref<TenantKey[]>([])
const tenants = ref<Tenant[]>([])
const stats = ref<TenantStat[]>([])
const tenantFilter = ref('')
const newTenantName = ref('')
const creatingTenant = ref(false)
const total = ref(0)
const query = ref('')
const orderBy = ref('issued_desc')
const PAGE = 20
const offset = ref(0)
const loading = ref(true)
const error = ref('')
const newRemark = ref('')
const newRole = ref('member')
const issueTenant = ref('')
const issuedKey = ref('')
const issuing = ref(false)
const revoking = ref('')

async function loadTenants() {
  try {
    const [t, st] = await Promise.all([api('/api/tenant/tenants'), api('/api/tenant/stats')])
    if (t.ok) tenants.value = (await t.json()) ?? []
    if (st.ok) stats.value = (await st.json()) ?? []
  } catch {
    /* tenant surfaces degrade to the key list alone */
  }
}

async function createTenant() {
  const name = newTenantName.value.trim()
  if (!name || creatingTenant.value) return
  creatingTenant.value = true
  error.value = ''
  try {
    const resp = await api('/api/tenant/tenants', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    newTenantName.value = ''
    await loadTenants()
  } catch (e) {
    error.value = String(e)
  } finally {
    creatingTenant.value = false
  }
}

async function load() {
  error.value = ''
  try {
    const params = new URLSearchParams({
      q: query.value.trim(),
      order: orderBy.value,
      limit: String(PAGE),
      offset: String(offset.value),
    })
    if (tenantFilter.value) params.set('tenant', tenantFilter.value)
    const usersResp = await api(`/api/tenant/users?${params}`)
    if (!usersResp.ok) throw new Error(`${usersResp.status}`)
    const doc = await usersResp.json()
    users.value = doc.items ?? []
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
function applyTenant() {
  offset.value = 0
  void load()
}

function sortBy(field: 'issued' | 'last_used') {
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

async function issue() {
  const remark = newRemark.value.trim()
  if (!remark || issuing.value) return
  if (!issueTenant.value) {
    error.value = t('users.needTenant')
    return
  }
  issuing.value = true
  error.value = ''
  issuedKey.value = ''
  try {
    const resp = await api('/api/tenant/users', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ remark, role: newRole.value, tenant_id: issueTenant.value }),
    })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    const doc = await resp.json()
    issuedKey.value = doc.key
    newRemark.value = ''
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    issuing.value = false
  }
}

async function revoke(u: TenantKey) {
  if (revoking.value) return
  if (!confirm(t('users.revokeConfirm', { remark: u.remark, prefix: u.prefix }))) return
  revoking.value = u.prefix
  error.value = ''
  try {
    const resp = await api(`/api/tenant/users/${encodeURIComponent(u.prefix)}`, { method: 'DELETE' })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    revoking.value = ''
  }
}

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}

onMounted(() => {
  void load()
  void loadTenants()
})
</script>

<template>
  <section class="users-card">
    <div class="users-title">{{ t('users.title') }}</div>
    <p class="users-hint">{{ t('users.hint') }}</p>
    <p v-if="error" class="users-error">{{ error }}</p>

    <input
      v-model="query"
      class="users-search"
      :placeholder="t('common.searchUserTenant')"
      @input="onSearch"
    />

    <div v-if="stats.length" class="users-stats">
      <div v-for="st in stats" :key="st.tenant_id" class="users-stat" @click="tenantFilter = tenantFilter === st.tenant_id ? '' : st.tenant_id; applyTenant()">
        <span class="users-stat-name">{{ st.tenant_name }}</span>
        <span class="users-stat-nums">{{ st.keys_active }} {{ t('users.statKeys') }} · {{ st.sessions }} {{ t('users.statSessions') }}</span>
        <span class="users-stat-used">{{ st.last_used ? fmtTime(st.last_used) : t('users.never') }}</span>
      </div>
    </div>

    <div class="users-issue">
      <select v-model="issueTenant" class="users-tenant-select" :disabled="issuing">
        <option value="" disabled>{{ t('users.pickTenant') }}</option>
        <option v-for="tn in tenants" :key="tn.id" :value="tn.id">{{ tn.name }}</option>
      </select>
      <input v-model="newTenantName" :placeholder="t('users.newTenantPlaceholder')" :disabled="creatingTenant" @keyup.enter="createTenant" />
      <button class="btn" :disabled="creatingTenant || !newTenantName.trim()" @click="createTenant">{{ t('users.newTenant') }}</button>
    </div>

    <div v-if="!tenants.length" class="users-hint">{{ t('users.needTenant') }}</div>

    <div class="users-issue">
      <input v-model="newRemark" :placeholder="t('users.remarkPlaceholder')" :disabled="issuing || !issueTenant" @keyup.enter="issue" />
      <select v-model="newRole" :disabled="issuing">
        <option value="admin">admin</option>
        <option value="member">member</option>
        <option value="viewer">viewer</option>
      </select>
      <button class="btn btn-primary" :disabled="issuing || !newRemark.trim()" @click="issue">
        {{ issuing ? t('users.issuing') : t('users.issue') }}
      </button>
    </div>
    <div v-if="issuedKey" class="users-token">
      <code>{{ issuedKey }}</code>
      <span class="users-token-once">{{ t('users.tokenOnce') }}</span>
    </div>

    <table v-if="users.length" class="users-table">
      <thead>
        <tr>
          <th>{{ t('users.colRemark') }}</th>
          <th>{{ t('users.colTenant') }}</th>
          <th>{{ t('users.colPrefix') }}</th>
          <th>{{ t('users.colRole') }}</th>
          <th class="sortable" @click="sortBy('issued')">{{ t('users.colIssued') }} {{ orderBy.startsWith('issued') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}</th>
          <th class="sortable" @click="sortBy('last_used')">{{ t('users.colLastUsed') }} {{ orderBy.startsWith('last_used') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}</th>
          <th>{{ t('users.colState') }}</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="u in users" :key="u.prefix" :class="{ revoked: u.revoked_at }">
          <td>{{ u.remark }}</td>
          <td>{{ u.tenant_name || u.tenant_id }}</td>
          <td><code :title="u.prefix" class="users-prefix">{{ u.prefix }}</code></td>
          <td><code>{{ u.role }}</code></td>
          <td>{{ fmtTime(u.issued_at) }}</td>
          <td>{{ u.last_used ? fmtTime(u.last_used) : t('users.never') }}</td>
          <td :class="u.revoked_at ? 'users-off' : 'users-live'">
            {{ u.revoked_at ? t('users.stateRevoked') + ' · ' + fmtTime(u.revoked_at) : '● ' + t('users.stateActive') }}
          </td>
          <td>
            <button v-if="!u.revoked_at" class="btn btn-danger" :disabled="revoking === u.prefix" @click="revoke(u)">
              {{ t('users.revoke') }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else-if="!loading" class="users-hint">{{ t('users.empty') }}</p>
    <p v-if="tenantFilter" class="users-hint">
      {{ t('users.filteredBy') }}{{ tenants.find((tn) => tn.id === tenantFilter)?.name ?? tenantFilter }}
      <a href="#" @click.prevent="tenantFilter = ''; applyTenant()">{{ t('users.clearFilter') }}</a>
    </p>

  </section>
</template>

<style scoped>
.users-card {
  border: 1px solid var(--border, #e4e7eb);
  border-radius: 10px;
  padding: 12px 14px;
}
.users-title {
  font-weight: 600;
  margin-bottom: 6px;
}
.users-hint {
  margin: 0 0 10px;
  font-size: 12.5px;
  opacity: 0.75;
}
.users-error {
  color: #e5534b;
  font-size: 12.5px;
  margin: 0 0 8px;
}
.users-search {
  width: 100%;
  padding: 7px 10px;
  margin-bottom: 10px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
}
.users-pager {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 10px;
  font-size: 12.5px;
}
.sortable {
  cursor: pointer;
  user-select: none;
}
.sortable:hover {
  opacity: 1;
}
.users-stats {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.users-stat {
  border: 1px solid var(--border, #e4e7eb);
  border-radius: 8px;
  padding: 6px 10px;
  font-size: 12px;
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.users-stat:hover {
  border-color: #2563eb;
}
.users-stat-name { font-weight: 600; }
.users-stat-nums { opacity: 0.75; }
.users-stat-used { opacity: 0.6; font-size: 11px; }
.users-tenant-select {
  max-width: 180px;
  padding: 7px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
}
.users-issue {
  display: flex;
  gap: 8px;
  margin-bottom: 10px;
}
.users-issue input {
  flex: 1;
  padding: 7px 10px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
}
.users-issue select {
  padding: 7px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
}
.users-token {
  margin: 0 0 10px;
  padding: 10px 12px;
  border: 1px dashed #2563eb;
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow-wrap: anywhere;
}
.users-token-once {
  font-size: 11.5px;
  color: #b45309;
}
.users-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
}
.users-table th {
  text-align: left;
  opacity: 0.6;
  font-weight: 500;
  padding: 4px 8px 4px 0;
}
.users-table td {
  padding: 5px 8px 5px 0;
  border-top: 1px solid var(--border, #e4e7eb);
}
.users-prefix {
  font-family: ui-monospace, monospace;
  font-size: 11px;
  opacity: 0.8;
}
.users-table tr.revoked td {
  opacity: 0.55;
}
.users-live {
  color: #047857;
}
.users-off {
  color: #e5534b;
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
.btn-primary {
  border-color: #2563eb;
  color: #2563eb;
}
.btn-danger {
  border-color: #c0392b;
  color: #c0392b;
}
</style>
