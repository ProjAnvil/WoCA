<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Ban, Check, Copy, KeyRound, RefreshCw } from 'lucide-vue-next'
import { injectOps } from '../inject'
import { pushToast } from '../composables/useToast'

// API 密钥 (product design v1.8 §6.7): the credential registry. A key is
// shown in full exactly once at issue time (dialog locks until the
// operator confirms it was saved); revocation is final and kills the
// key's sessions on the spot — single or batch. Search, tenant and state
// filters, sorting and paging all run server-side; any filter change
// drops the selection.
const { t } = useI18n()
const { api } = injectOps()

interface TenantKey {
  prefix: string
  remark: string
  role: string
  tenant_id: string
  tenant_name: string
  issued_at: number
  last_used?: number
  revoked_at?: number
}
interface Tenant {
  id: string
  name: string
}

const users = ref<TenantKey[]>([])
const tenants = ref<Tenant[]>([])
const total = ref(0)
const query = ref('')
const tenantFilter = ref('')
const stateFilter = ref('')
const orderBy = ref('issued_desc')
const PAGE = 20
const offset = ref(0)
const loading = ref(true)
const error = ref('')
const selected = ref(new Set<string>())

const showIssue = ref(false)
const issueStage = ref<'form' | 'key'>('form')
const issueTenant = ref('')
const issueRemark = ref('')
const issueRole = ref('member')
const issueError = ref('')
const issuing = ref(false)
const issuedKey = ref('')
const copied = ref(false)

const revokeTargets = ref<TenantKey[]>([])
const revoking = ref(false)

async function loadTenants() {
  try {
    const resp = await api('/api/tenant/tenants')
    if (resp.ok) tenants.value = (await resp.json()) ?? []
  } catch {
    /* the key list alone still works without the filter vocabulary */
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
    if (stateFilter.value) params.set('state', stateFilter.value)
    const resp = await api(`/api/tenant/users?${params}`)
    if (resp.status === 401) return
    if (!resp.ok) throw new Error(`${resp.status}`)
    const doc = await resp.json()
    users.value = doc.items ?? []
    total.value = doc.total ?? 0
  } catch (e) {
    error.value = String(e)
  } finally {
    loading.value = false
  }
}

// Any view change drops the selection — the conservative default for
// paged bulk operations.
function reloadView() {
  offset.value = 0
  selected.value.clear()
  void load()
}

let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(reloadView, 250)
}

const page = computed(() => Math.floor(offset.value / PAGE) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE)))
function prevPage() {
  offset.value = Math.max(0, offset.value - PAGE)
  selected.value.clear()
  void load()
}
function nextPage() {
  if (offset.value + PAGE < total.value) {
    offset.value += PAGE
    selected.value.clear()
    void load()
  }
}
function sortBy(field: 'issued' | 'last_used') {
  orderBy.value = orderBy.value === field ? `${field}_desc` : field
  reloadView()
}

const selectable = computed(() => users.value.filter((u) => !u.revoked_at))
const hasSelection = computed(() => selected.value.size > 0)
const allSelected = computed(
  () => selectable.value.length > 0 && selectable.value.every((u) => selected.value.has(u.prefix)),
)
function toggleRow(u: TenantKey, on: boolean) {
  if (on) selected.value.add(u.prefix)
  else selected.value.delete(u.prefix)
}
function toggleAll() {
  if (allSelected.value) selected.value.clear()
  else selectable.value.forEach((u) => selected.value.add(u.prefix))
}

function openIssue() {
  issueStage.value = 'form'
  issueTenant.value = ''
  issueRemark.value = ''
  issueRole.value = 'member'
  issueError.value = ''
  issuedKey.value = ''
  copied.value = false
  showIssue.value = true
}

async function issue() {
  const remark = issueRemark.value.trim()
  if (!remark || issuing.value) return
  if (!issueTenant.value) {
    issueError.value = t('keys.needTenant')
    return
  }
  issuing.value = true
  issueError.value = ''
  try {
    const resp = await api('/api/tenant/users', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ remark, role: issueRole.value, tenant_id: issueTenant.value }),
    })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    const doc = await resp.json()
    issuedKey.value = doc.key
    issueStage.value = 'key'
    await load()
  } catch (e) {
    issueError.value = String(e)
  } finally {
    issuing.value = false
  }
}

async function copyIssued() {
  try {
    await navigator.clipboard.writeText(issuedKey.value)
    copied.value = true
    pushToast(t('common.copied'), 'success')
  } catch {
    /* clipboard unavailable (insecure context) — manual copy remains */
  }
}

function askBatchRevoke() {
  const prefixes = [...selected.value]
  revokeTargets.value = users.value.filter((u) => prefixes.includes(u.prefix))
}
function askRowRevoke(u: TenantKey) {
  revokeTargets.value = [u]
}

async function confirmRevoke() {
  const targets = revokeTargets.value
  if (!targets.length || revoking.value) return
  revoking.value = true
  error.value = ''
  try {
    const resp = await api('/api/tenant/users/revoke', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ prefixes: targets.map((u) => u.prefix) }),
    })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    const doc = await resp.json()
    pushToast(t('keys.revokedToast', { count: doc.revoked ?? 0 }), 'success')
    revokeTargets.value = []
    selected.value.clear()
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    revoking.value = false
  }
}

function onKeydown(e: KeyboardEvent) {
  if (e.key !== 'Escape') return
  // The one-time key stage cannot be dismissed with Esc — losing the
  // key means re-issuing a credential.
  if (showIssue.value && issueStage.value === 'form' && !issuing.value) showIssue.value = false
  else if (revokeTargets.value.length && !revoking.value) revokeTargets.value = []
}
onMounted(() => {
  void load()
  void loadTenants()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))

function fmtTime(ms: number | undefined): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}
</script>

<template>
  <section>
    <div class="sec-head">
      <div>
        <div class="sec-title">{{ t('keys.title') }}</div>
        <p class="sec-hint">{{ t('keys.hint') }}</p>
      </div>
      <div class="sec-actions">
        <button class="btn btn-sm btn-ghost" :disabled="loading" @click="load">
          <RefreshCw :size="13" /> {{ t('common.refresh') }}
        </button>
        <button class="btn btn-sm btn-primary" @click="openIssue">
          <KeyRound :size="14" /> {{ t('keys.issue') }}
        </button>
      </div>
    </div>

    <p v-if="error" class="alert alert-error ops-alert">{{ error }}</p>

    <div v-if="!hasSelection" class="toolbar">
      <input
        v-model="query"
        type="search"
        class="input input-sm ops-search"
        :placeholder="t('common.searchUserTenant')"
        @input="onSearch"
      />
      <select v-model="tenantFilter" class="select select-sm" @change="reloadView">
        <option value="">{{ t('keys.filterAllTenants') }}</option>
        <option v-for="tn in tenants" :key="tn.id" :value="tn.id">{{ tn.name }}</option>
      </select>
      <select v-model="stateFilter" class="select select-sm" @change="reloadView">
        <option value="">{{ t('keys.filterAllStates') }}</option>
        <option value="active">{{ t('keys.stateActive') }}</option>
        <option value="revoked">{{ t('keys.stateRevoked') }}</option>
      </select>
    </div>
    <div v-else class="toolbar ops-batchbar">
      <span class="ops-batchbar-count">{{ t('common.selectedCount', { n: selected.size }) }}</span>
      <div class="ops-batchbar-actions">
        <button class="btn btn-sm btn-error" @click="askBatchRevoke">
          <Ban :size="13" /> {{ t('keys.batchRevoke') }}
        </button>
        <button class="btn btn-sm btn-ghost" @click="selected.clear()">{{ t('common.clearSelection') }}</button>
      </div>
    </div>

    <div class="card ops-card">
      <table v-if="users.length" class="table table-sm ops-table" :class="{ 'ops-loading': loading }">
        <thead>
          <tr>
            <th class="ops-col-check">
              <input type="checkbox" class="checkbox checkbox-xs" :checked="allSelected" @change="toggleAll" />
            </th>
            <th>{{ t('keys.colRemark') }}</th>
            <th>{{ t('keys.colTenant') }}</th>
            <th>{{ t('keys.colPrefix') }}</th>
            <th>{{ t('keys.colRole') }}</th>
            <th
              class="sortable"
              :aria-sort="orderBy.startsWith('issued') ? (orderBy.endsWith('_desc') ? 'descending' : 'ascending') : 'none'"
              @click="sortBy('issued')"
            >
              {{ t('keys.colIssued') }} {{ orderBy.startsWith('issued') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}
            </th>
            <th
              class="sortable"
              :aria-sort="orderBy.startsWith('last_used') ? (orderBy.endsWith('_desc') ? 'descending' : 'ascending') : 'none'"
              @click="sortBy('last_used')"
            >
              {{ t('keys.colLastUsed') }} {{ orderBy.startsWith('last_used') ? (orderBy.endsWith('_desc') ? '↓' : '↑') : '' }}
            </th>
            <th>{{ t('keys.colState') }}</th>
            <th class="ops-col-actions"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.prefix" :class="{ 'ops-row-revoked': u.revoked_at }">
            <td class="ops-col-check">
              <input
                v-if="!u.revoked_at"
                type="checkbox"
                class="checkbox checkbox-xs"
                :checked="selected.has(u.prefix)"
                @change="toggleRow(u, ($event.target as HTMLInputElement).checked)"
              />
            </td>
            <td>{{ u.remark }}</td>
            <td>{{ u.tenant_name || u.tenant_id }}</td>
            <td><code class="ops-mono" :title="u.prefix">{{ u.prefix }}</code></td>
            <td><code class="ops-mono">{{ u.role }}</code></td>
            <td>{{ fmtTime(u.issued_at) }}</td>
            <td>{{ u.last_used ? fmtTime(u.last_used) : t('common.never') }}</td>
            <td>
              <span v-if="u.revoked_at" class="ops-state ops-state-revoked">○ {{ t('keys.stateRevoked') }}</span>
              <span v-else class="ops-state">● {{ t('keys.stateActive') }}</span>
            </td>
            <td class="ops-col-actions">
              <button v-if="!u.revoked_at" class="btn btn-xs btn-ghost" @click="askRowRevoke(u)">
                {{ t('keys.revoke') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-else-if="loading" class="ops-skeleton">
        <div v-for="i in 5" :key="i" class="skeleton ops-skeleton-row"></div>
      </div>
      <div v-else-if="query || tenantFilter || stateFilter" class="ops-empty">
        {{ t('keys.emptyFiltered') }}
        <button
          class="btn btn-xs btn-ghost mt-2"
          @click="query = ''; tenantFilter = ''; stateFilter = ''; reloadView()"
        >
          {{ t('keys.clearFilters') }}
        </button>
      </div>
      <div v-else class="ops-empty">
        <div>{{ t('keys.empty') }}</div>
        <button class="btn btn-sm btn-primary mt-3" @click="openIssue">
          <KeyRound :size="14" /> {{ t('keys.issue') }}
        </button>
      </div>
    </div>

    <div v-if="total > PAGE || page > 1" class="ops-pager">
      <button class="btn btn-xs btn-ghost" :disabled="offset === 0" @click="prevPage">‹</button>
      <span>{{ t('keys.pageOf', { page, pages }) }} · {{ t('keys.totalCount', { total }) }}</span>
      <button class="btn btn-xs btn-ghost" :disabled="offset + PAGE >= total" @click="nextPage">›</button>
    </div>

    <div v-if="showIssue" class="modal modal-open" role="dialog" :aria-label="t('keys.issueTitle')">
      <div class="modal-box ops-modal-box">
        <template v-if="issueStage === 'form'">
          <div class="ops-modal-title">{{ t('keys.issueTitle') }}</div>
          <label class="form-control">
            <div class="label-text">{{ t('keys.tenantLabel') }}</div>
            <select v-model="issueTenant" class="select select-sm mt-1" :disabled="issuing">
              <option value="" disabled>{{ t('keys.pickTenant') }}</option>
              <option v-for="tn in tenants" :key="tn.id" :value="tn.id">{{ tn.name }}</option>
            </select>
          </label>
          <label class="form-control">
            <div class="label-text">{{ t('keys.remarkLabel') }}</div>
            <input
              v-model="issueRemark"
              class="input input-sm mt-1"
              :placeholder="t('keys.remarkPlaceholder')"
              :disabled="issuing"
              @keyup.enter="issue"
            />
          </label>
          <label class="form-control">
            <div class="label-text">{{ t('keys.roleLabel') }}</div>
            <select v-model="issueRole" class="select select-sm mt-1" :disabled="issuing">
              <option value="admin">admin</option>
              <option value="member">member</option>
              <option value="viewer">viewer</option>
            </select>
          </label>
          <p v-if="issueError" class="ops-error">{{ issueError }}</p>
          <div class="ops-modal-actions">
            <button class="btn btn-sm btn-ghost" :disabled="issuing" @click="showIssue = false">
              {{ t('common.cancel') }}
            </button>
            <button class="btn btn-sm btn-primary" :disabled="issuing || !issueRemark.trim()" @click="issue">
              {{ issuing ? t('keys.issuing') : t('keys.issue') }}
            </button>
          </div>
        </template>
        <template v-else>
          <div class="ops-modal-title">{{ t('keys.issuedTitle') }}</div>
          <p class="ops-once-warn">{{ t('keys.tokenOnceWarning') }}</p>
          <div class="ops-keybox">
            <code class="ops-keytext">{{ issuedKey }}</code>
            <button class="btn btn-xs btn-ghost" @click="copyIssued">
              <Copy v-if="!copied" :size="13" />
              <Check v-else :size="13" />
              {{ t('common.copy') }}
            </button>
          </div>
          <p class="ops-once-hint">{{ t('keys.tokenOnceHint') }}</p>
          <div class="ops-modal-actions">
            <button class="btn btn-sm btn-primary" @click="showIssue = false">{{ t('keys.tokenSaved') }}</button>
          </div>
        </template>
      </div>
    </div>

    <div v-if="revokeTargets.length" class="modal modal-open" role="dialog" :aria-label="t('keys.revokeTitle')">
      <div class="modal-box ops-modal-box">
        <div class="ops-modal-title">{{ t('keys.revokeTitle') }}</div>
        <p class="ops-warn-text">
          {{ t('keys.revokeBody', {
            count: revokeTargets.length,
            names: revokeTargets.map((u) => u.remark).join(', '),
          }) }}
        </p>
        <div class="ops-modal-actions">
          <button class="btn btn-sm btn-ghost" :disabled="revoking" @click="revokeTargets = []">
            {{ t('common.cancel') }}
          </button>
          <button class="btn btn-sm btn-primary" :disabled="revoking" @click="confirmRevoke">
            {{ revoking ? t('keys.revoking') : t('keys.revokeAction', { count: revokeTargets.length }) }}
          </button>
        </div>
      </div>
      <div class="modal-backdrop" @click="!revoking && (revokeTargets = [])"></div>
    </div>
  </section>
</template>

<style scoped>
.sec-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 10px; }
.sec-title { font-weight: 600; }
.sec-hint { margin: 2px 0 0; font-size: 12.5px; opacity: 0.7; }
.sec-actions { display: flex; gap: 8px; flex: none; }
.toolbar { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; min-height: 34px; }
.ops-search { max-width: 340px; width: 100%; }
.ops-batchbar { justify-content: space-between; }
.ops-batchbar-count { font-size: 13px; font-weight: 600; }
.ops-batchbar-actions { display: flex; gap: 8px; }
.ops-card { padding: 6px 12px; }
.ops-table { font-size: 13px; }
.ops-loading { opacity: 0.6; }
.ops-table :deep(th) { font-size: 12px; font-weight: 500; color: #616e7c; }
.ops-table :deep(td) { padding-top: 8px; padding-bottom: 8px; }
.ops-col-check { width: 30px; }
.ops-col-actions { width: 70px; text-align: right; }
.ops-row-revoked td { opacity: 0.55; }
.ops-state { color: #1f2933; }
.ops-state-revoked { color: #616e7c; }
.ops-mono { font-family: ui-monospace, monospace; font-size: 11.5px; opacity: 0.8; }
.sortable { cursor: pointer; user-select: none; }
.ops-skeleton { display: flex; flex-direction: column; gap: 10px; padding: 10px 0; }
.ops-skeleton-row { height: 32px; width: 100%; }
.ops-empty { padding: 44px 0; text-align: center; font-size: 13px; color: #616e7c; }
.ops-pager { display: flex; align-items: center; gap: 10px; font-size: 12.5px; color: #616e7c; }
.ops-alert { font-size: 12.5px; padding: 8px 12px; }
.ops-modal-box { max-width: 480px; font-size: 13.5px; }
.ops-modal-title { font-weight: 600; margin-bottom: 10px; }
.ops-modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
.ops-error { color: #c0392b; font-size: 12.5px; margin: 6px 0 0; }
.ops-warn-text { margin: 0; line-height: 1.6; }
.label-text { font-size: 12.5px; color: #616e7c; }
.form-control { margin-top: 10px; }
.ops-once-warn { margin: 0 0 10px; font-weight: 600; }
.ops-keybox {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 12px;
  border: 1px dashed #111;
  border-radius: 8px;
  overflow-wrap: anywhere;
}
.ops-keytext { font-family: ui-monospace, monospace; font-size: 12px; flex: 1; }
.ops-once-hint { margin: 10px 0 0; font-size: 12.5px; color: #616e7c; }
</style>
