<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Lock, Plus, RefreshCw, Trash2 } from 'lucide-vue-next'
import { injectOps } from '../inject'
import { pushToast } from '../composables/useToast'

// 租户管理 (product design v1.8 §6.7): the business-unit registry with
// per-unit scale columns (active keys, live sign-ins, last use) and the
// terminal delete — deleting a unit cascades (its keys and users are
// removed, live sessions die on the spot; no grace). The platform tenant
// ("local", the seeded machine identity) is locked. Search filters the
// client-side list; creation and deletion live in dialogs, never inline.
const { t } = useI18n()
const { api } = injectOps()

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
interface Row {
  id: string
  name: string
  created_at: number
  keys_active: number
  keys_revoked: number
  sessions: number
  last_used: number
  locked: boolean
}

const rows = ref<Row[]>([])
const query = ref('')
const loading = ref(true)
const error = ref('')
const selected = ref(new Set<string>())

const showCreate = ref(false)
const newName = ref('')
const creating = ref(false)
const createError = ref('')

const deleteTargets = ref<Row[]>([])
const confirmCount = ref('')
const deleting = ref(false)

const PLATFORM_TENANT = 'local'

async function load() {
  error.value = ''
  try {
    const [tenantsResp, statsResp] = await Promise.all([api('/api/tenant/tenants'), api('/api/tenant/stats')])
    if (tenantsResp.status === 401) return
    if (!tenantsResp.ok || !statsResp.ok) throw new Error(`${tenantsResp.status}`)
    const tenants: Tenant[] = (await tenantsResp.json()) ?? []
    const stats: TenantStat[] = (await statsResp.json()) ?? []
    const statByID = new Map(stats.map((st) => [st.tenant_id, st]))
    rows.value = tenants.map((tn) => ({
      id: tn.id,
      name: tn.name,
      created_at: tn.created_at,
      keys_active: statByID.get(tn.id)?.keys_active ?? 0,
      keys_revoked: statByID.get(tn.id)?.keys_revoked ?? 0,
      sessions: statByID.get(tn.id)?.sessions ?? 0,
      last_used: statByID.get(tn.id)?.last_used ?? 0,
      locked: tn.id === PLATFORM_TENANT,
    }))
  } catch (e) {
    error.value = String(e)
  } finally {
    loading.value = false
  }
}

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return rows.value
  return rows.value.filter((r) => r.name.toLowerCase().includes(q) || r.id.toLowerCase().includes(q))
})
const selectable = computed(() => filtered.value.filter((r) => !r.locked))
const hasSelection = computed(() => selected.value.size > 0)
const allSelected = computed(
  () => selectable.value.length > 0 && selectable.value.every((r) => selected.value.has(r.id)),
)

function toggleRow(row: Row, on: boolean) {
  if (on) selected.value.add(row.id)
  else selected.value.delete(row.id)
}
function toggleAll() {
  if (allSelected.value) selected.value.clear()
  else selectable.value.forEach((r) => selected.value.add(r.id))
}
// Any change of the visible set silently drops the selection (the
// conservative default for filtered bulk operations).
watch(query, () => selected.value.clear())

const needCountConfirm = computed(() => deleteTargets.value.length >= 5)
const deleteAggregate = computed(() => {
  const targets = deleteTargets.value
  const keys = targets.reduce((sum, r) => sum + r.keys_active + r.keys_revoked, 0)
  const sessions = targets.reduce((sum, r) => sum + r.sessions, 0)
  return { keys, sessions }
})
const countConfirmOk = computed(
  () => !needCountConfirm.value || confirmCount.value.trim() === String(deleteTargets.value.length),
)

function openCreate() {
  createError.value = ''
  newName.value = ''
  showCreate.value = true
}

async function create() {
  const name = newName.value.trim()
  if (!name || creating.value) return
  creating.value = true
  createError.value = ''
  try {
    const resp = await api('/api/tenant/tenants', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    showCreate.value = false
    pushToast(t('tenants.createdToast', { name }), 'success')
    await load()
  } catch (e) {
    createError.value = String(e)
  } finally {
    creating.value = false
  }
}

function askBatchDelete() {
  const ids = [...selected.value]
  deleteTargets.value = rows.value.filter((r) => ids.includes(r.id))
  confirmCount.value = ''
}
function askRowDelete(row: Row) {
  deleteTargets.value = [row]
  confirmCount.value = ''
}

async function confirmDelete() {
  const targets = deleteTargets.value
  if (!targets.length || deleting.value || !countConfirmOk.value) return
  deleting.value = true
  error.value = ''
  try {
    const resp = await api('/api/tenant/tenants/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids: targets.map((r) => r.id) }),
    })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    const doc = await resp.json()
    const deleted: string[] = doc.deleted ?? []
    const blocked: string[] = doc.blocked ?? []
    const missing: string[] = doc.missing ?? []
    if (deleted.length) pushToast(t('tenants.deletedToast', { count: deleted.length }), 'success')
    if (blocked.length) pushToast(t('tenants.blockedToast', { count: blocked.length }), 'error')
    if (missing.length) pushToast(t('tenants.missingToast', { count: missing.length }), 'info')
    deleteTargets.value = []
    selected.value.clear()
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    deleting.value = false
  }
}

function onKeydown(e: KeyboardEvent) {
  if (e.key !== 'Escape') return
  if (showCreate.value && !creating.value) showCreate.value = false
  else if (deleteTargets.value.length && !deleting.value) deleteTargets.value = []
}
onMounted(() => {
  void load()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}
</script>

<template>
  <section>
    <div class="sec-head">
      <div>
        <div class="sec-title">{{ t('tenants.title') }}</div>
        <p class="sec-hint">{{ t('tenants.hint') }}</p>
      </div>
      <div class="sec-actions">
        <button class="btn btn-sm btn-ghost" :disabled="loading" @click="load">
          <RefreshCw :size="13" /> {{ t('common.refresh') }}
        </button>
        <button class="btn btn-sm btn-primary" @click="openCreate">
          <Plus :size="14" /> {{ t('tenants.newTenant') }}
        </button>
      </div>
    </div>

    <p v-if="error" class="alert alert-error ops-alert">{{ error }}</p>

    <div v-if="!hasSelection" class="toolbar">
      <input
        v-model="query"
        type="search"
        class="input input-sm ops-search"
        :placeholder="t('tenants.searchPlaceholder')"
      />
    </div>
    <div v-else class="toolbar ops-batchbar">
      <span class="ops-batchbar-count">{{ t('common.selectedCount', { n: selected.size }) }}</span>
      <div class="ops-batchbar-actions">
        <button class="btn btn-sm btn-error" @click="askBatchDelete">
          <Trash2 :size="13" /> {{ t('tenants.batchDelete') }}
        </button>
        <button class="btn btn-sm btn-ghost" @click="selected.clear()">{{ t('common.clearSelection') }}</button>
      </div>
    </div>

    <div class="card ops-card">
      <table v-if="filtered.length" class="table table-sm ops-table">
        <thead>
          <tr>
            <th class="ops-col-check">
              <input type="checkbox" class="checkbox checkbox-xs" :checked="allSelected" @change="toggleAll" />
            </th>
            <th>{{ t('tenants.colName') }}</th>
            <th>{{ t('tenants.colId') }}</th>
            <th>{{ t('tenants.colCreated') }}</th>
            <th class="text-right">{{ t('tenants.colKeysActive') }}</th>
            <th class="text-right">{{ t('tenants.colSessions') }}</th>
            <th>{{ t('tenants.colLastUsed') }}</th>
            <th class="ops-col-actions"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in filtered" :key="row.id" :class="{ 'ops-row-locked': row.locked }">
            <td class="ops-col-check">
              <input
                v-if="!row.locked"
                type="checkbox"
                class="checkbox checkbox-xs"
                :checked="selected.has(row.id)"
                @change="toggleRow(row, ($event.target as HTMLInputElement).checked)"
              />
            </td>
            <td>
              <span v-if="row.locked" class="tooltip" :data-tip="t('tenants.lockReason')">
                <Lock :size="12" class="ops-lock" /> {{ row.name }}
              </span>
              <span v-else>{{ row.name }}</span>
            </td>
            <td><code class="ops-mono">{{ row.id }}</code></td>
            <td>{{ fmtTime(row.created_at) }}</td>
            <td class="text-right">{{ row.keys_active }}</td>
            <td class="text-right">{{ row.sessions }}</td>
            <td>{{ row.last_used ? fmtTime(row.last_used) : t('common.never') }}</td>
            <td class="ops-col-actions">
              <button v-if="!row.locked" class="btn btn-xs btn-ghost" @click="askRowDelete(row)">
                {{ t('common.delete') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-else-if="loading" class="ops-skeleton">
        <div v-for="i in 5" :key="i" class="skeleton ops-skeleton-row"></div>
      </div>
      <div v-else-if="rows.length" class="ops-empty">
        {{ t('tenants.emptyFiltered') }}
        <button class="btn btn-xs btn-ghost mt-2" @click="query = ''">{{ t('keys.clearFilters') }}</button>
      </div>
      <div v-else class="ops-empty">
        <div>{{ t('tenants.empty') }}</div>
        <button class="btn btn-sm btn-primary mt-3" @click="openCreate">
          <Plus :size="14" /> {{ t('tenants.newTenant') }}
        </button>
      </div>
    </div>

    <div v-if="showCreate" class="modal modal-open" role="dialog" :aria-label="t('tenants.newTenant')">
      <div class="modal-box ops-modal-box">
        <div class="ops-modal-title">{{ t('tenants.newTenant') }}</div>
        <label class="form-control">
          <div class="label-text">{{ t('tenants.nameLabel') }}</div>
          <input
            ref="nameInput"
            v-model="newName"
            class="input input-sm mt-1"
            :placeholder="t('tenants.namePlaceholder')"
            :disabled="creating"
            @keyup.enter="create"
          />
        </label>
        <p v-if="createError" class="ops-error">{{ createError }}</p>
        <div class="ops-modal-actions">
          <button class="btn btn-sm btn-ghost" :disabled="creating" @click="showCreate = false">
            {{ t('common.cancel') }}
          </button>
          <button class="btn btn-sm btn-primary" :disabled="creating || !newName.trim()" @click="create">
            {{ creating ? t('common.creating') : t('common.create') }}
          </button>
        </div>
      </div>
      <div class="modal-backdrop" @click="!creating && (showCreate = false)"></div>
    </div>

    <div v-if="deleteTargets.length" class="modal modal-open" role="dialog" :aria-label="t('tenants.deleteTitle')">
      <div class="modal-box ops-modal-box">
        <div class="ops-modal-title">{{ t('tenants.deleteTitle') }}</div>
        <p class="ops-warn-text">
          {{ t('tenants.deleteBody', {
            count: deleteTargets.length,
            names: deleteTargets.map((r) => r.name).join(', '),
            keys: deleteAggregate.keys,
            sessions: deleteAggregate.sessions,
          }) }}
        </p>
        <div v-if="needCountConfirm" class="form-control">
          <div class="label-text">{{ t('tenants.typeCount', { count: deleteTargets.length }) }}</div>
          <input v-model="confirmCount" class="input input-sm mt-1" :disabled="deleting" />
        </div>
        <p v-if="needCountConfirm && !countConfirmOk && confirmCount" class="ops-error">
          {{ t('tenants.countMismatch') }}
        </p>
        <div class="ops-modal-actions">
          <button class="btn btn-sm btn-ghost" :disabled="deleting" @click="deleteTargets = []">
            {{ t('common.cancel') }}
          </button>
          <button class="btn btn-sm btn-primary" :disabled="deleting || !countConfirmOk" @click="confirmDelete">
            {{ deleting ? t('common.deleting') : t('tenants.deleteAction', { count: deleteTargets.length }) }}
          </button>
        </div>
      </div>
      <div class="modal-backdrop" @click="!deleting && (deleteTargets = [])"></div>
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
.ops-table :deep(th) { font-size: 12px; font-weight: 500; color: #616e7c; }
.ops-table :deep(td) { padding-top: 8px; padding-bottom: 8px; }
.ops-col-check { width: 30px; }
.ops-col-actions { width: 70px; text-align: right; }
.ops-row-locked td { opacity: 0.6; }
.ops-lock { vertical-align: -2px; }
.ops-mono { font-family: ui-monospace, monospace; font-size: 11.5px; opacity: 0.8; }
.ops-skeleton { display: flex; flex-direction: column; gap: 10px; padding: 10px 0; }
.ops-skeleton-row { height: 32px; width: 100%; }
.ops-empty { padding: 44px 0; text-align: center; font-size: 13px; color: #616e7c; }
.ops-alert { font-size: 12.5px; padding: 8px 12px; }
.ops-modal-box { max-width: 440px; font-size: 13.5px; }
.ops-modal-title { font-weight: 600; margin-bottom: 10px; }
.ops-modal-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
.ops-error { color: #c0392b; font-size: 12.5px; margin: 6px 0 0; }
.ops-warn-text { margin: 0; line-height: 1.6; }
.label-text { font-size: 12.5px; color: #616e7c; }
.form-control { margin-top: 10px; }
</style>
