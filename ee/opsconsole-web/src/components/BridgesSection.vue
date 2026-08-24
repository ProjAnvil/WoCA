<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectOps } from '../inject'

// Bridge enrollment (product design §6.7): one-time token issuance
// (shown exactly once), worker-led approvals, the registration inventory
// with one-click revocation, and the edge SPKI pin policy (k3s-mode
// anti-poisoning, embedded in issued tokens).
const { t } = useI18n()
const { api } = injectOps()

const edgePin = ref('')
const pinSaved = ref(false)
const busy = ref(false)
const error = ref('')

interface IssuedToken {
  enrollment_token: string
  expires_in_s: number
}
interface Pending {
  fingerprint: string
  created_at: number
}
interface Bridge {
  bridge_id: string
  created_at: number
  last_seen: number
  live: boolean
}

const issued = ref<IssuedToken | null>(null)
const issuing = ref(false)
const pending = ref<Pending[]>([])
const bridges = ref<Bridge[]>([])
const revoking = ref('')

async function load() {
  error.value = ''
  try {
    const [settings, pend, inv] = await Promise.all([
      api('/api/gw/settings'),
      api('/api/gw/pending'),
      api('/api/gw/bridges'),
    ])
    if (settings.ok) {
      const doc = await settings.json()
      edgePin.value = doc.edge_spki_pin ?? ''
    } else if (settings.status !== 401) {
      error.value = `${settings.status}`
    }
    if (pend.ok) {
      pending.value = (await pend.json()) ?? []
    } else if (pend.status !== 401) {
      error.value = error.value || `${pend.status}`
    }
    if (inv.ok) {
      bridges.value = (await inv.json()) ?? []
    } else if (inv.status !== 401) {
      error.value = error.value || `${inv.status}`
    }
  } catch (e) {
    error.value = String(e)
  }
}

async function savePin() {
  busy.value = true
  error.value = ''
  pinSaved.value = false
  try {
    const resp = await api('/api/gw/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ edge_spki_pin: edgePin.value.trim() }),
    })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({ error: resp.status }))
      throw new Error(doc.error ?? `${resp.status}`)
    }
    pinSaved.value = true
  } catch (e) {
    error.value = String(e)
  } finally {
    busy.value = false
  }
}

async function issueToken() {
  if (issuing.value) return
  issuing.value = true
  error.value = ''
  issued.value = null
  try {
    const resp = await api('/api/gw/enrollments', { method: 'POST' })
    if (!resp.ok) throw new Error(`${resp.status}`)
    issued.value = await resp.json()
  } catch (e) {
    error.value = String(e)
  } finally {
    issuing.value = false
  }
}

async function approve(fp: string) {
  error.value = ''
  try {
    const resp = await api(`/api/gw/pending/${encodeURIComponent(fp)}/approve`, { method: 'POST' })
    if (!resp.ok) throw new Error(`${resp.status}`)
    await load()
  } catch (e) {
    error.value = String(e)
  }
}

async function revoke(id: string) {
  if (revoking.value) return
  if (!confirm(t('bridges.revokeConfirm', { id }))) return
  revoking.value = id
  error.value = ''
  try {
    const resp = await api(`/api/gw/bridges/${encodeURIComponent(id)}`, { method: 'DELETE' })
    if (!resp.ok) throw new Error(`${resp.status}`)
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

onMounted(load)
</script>

<template>
  <section class="gw-card">
    <p v-if="error" class="gw-error">{{ error }}</p>

    <div class="gw-block">
      <div class="gw-title">{{ t('bridges.issueTitle') }}</div>
      <p class="gw-hint">{{ t('bridges.issueHint') }}</p>
      <button class="btn btn-primary" :disabled="issuing" @click="issueToken">
        {{ issuing ? t('bridges.issuing') : t('bridges.issue') }}
      </button>
      <div v-if="issued" class="gw-token">
        <code>{{ issued.enrollment_token }}</code>
        <span class="gw-token-once">{{ t('bridges.tokenOnce', { s: Math.round(issued.expires_in_s / 3600) }) }}</span>
      </div>
    </div>

    <div class="gw-block">
      <div class="gw-title">{{ t('bridges.pendingTitle') }}</div>
      <p class="gw-hint">{{ t('bridges.pendingHint') }}</p>
      <table v-if="pending.length" class="gw-table">
        <thead>
          <tr>
            <th>fingerprint</th>
            <th>{{ t('audit.when') }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in pending" :key="p.fingerprint">
            <td><code>{{ p.fingerprint }}</code></td>
            <td>{{ fmtTime(p.created_at) }}</td>
            <td><button class="btn btn-primary" @click="approve(p.fingerprint)">{{ t('bridges.approve') }}</button></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="gw-hint">{{ t('bridges.noPending') }}</p>
    </div>

    <div class="gw-block">
      <div class="gw-title">{{ t('bridges.bridgesTitle') }}</div>
      <p class="gw-hint">{{ t('bridges.bridgesHint') }}</p>
      <table v-if="bridges.length" class="gw-table">
        <thead>
          <tr>
            <th>{{ t('bridges.colBridge') }}</th>
            <th>{{ t('bridges.colRegistered') }}</th>
            <th>{{ t('bridges.colLastSeen') }}</th>
            <th>{{ t('bridges.colState') }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="b in bridges" :key="b.bridge_id">
            <td><code>{{ b.bridge_id }}</code></td>
            <td>{{ fmtTime(b.created_at) }}</td>
            <td>{{ fmtTime(b.last_seen) }}</td>
            <td :class="b.live ? 'gw-live' : 'gw-off'">
              {{ b.live ? '● ' + t('bridges.live') : t('bridges.offline') }}
            </td>
            <td>
              <button class="btn btn-danger" :disabled="revoking === b.bridge_id" @click="revoke(b.bridge_id)">
                {{ t('bridges.revoke') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="gw-hint">{{ t('bridges.noBridges') }}</p>
    </div>

    <div class="gw-block">
      <div class="gw-title">{{ t('bridges.edgePin') }}</div>
      <p class="gw-hint">{{ t('bridges.edgePinHint') }}</p>
      <div class="gw-pin-row">
        <input v-model="edgePin" class="mono" :placeholder="t('bridges.edgePinPlaceholder')" />
        <button class="btn btn-primary" :disabled="busy" @click="savePin">{{ t('bridges.pinSave') }}</button>
        <span v-if="pinSaved" class="gw-live">{{ t('bridges.pinSaved') }}</span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.gw-card {
  border: 1px solid var(--border, #e4e7eb);
  border-radius: 10px;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.gw-block {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.gw-title {
  font-weight: 600;
}
.gw-hint {
  margin: 0;
  font-size: 12.5px;
  opacity: 0.75;
}
.gw-error {
  color: #e5534b;
  font-size: 12.5px;
  margin: 0;
}
.gw-token {
  margin-top: 6px;
  padding: 10px 12px;
  border: 1px dashed #2563eb;
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow-wrap: anywhere;
}
.gw-token-once {
  font-size: 11.5px;
  color: #b45309;
}
.gw-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
}
.gw-table th {
  text-align: left;
  opacity: 0.6;
  font-weight: 500;
  padding: 4px 8px 4px 0;
}
.gw-table td {
  padding: 5px 8px 5px 0;
  border-top: 1px solid var(--border, #e4e7eb);
}
.gw-live {
  color: #047857;
  font-size: 12.5px;
}
.gw-off {
  color: #b45309;
}
.gw-pin-row {
  display: flex;
  gap: 8px;
  align-items: center;
}
.gw-pin-row input {
  flex: 1;
  padding: 7px 10px;
  border: 1px solid var(--border, #cbd2d9);
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
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
  align-self: flex-start;
}
.btn-danger {
  border-color: #c0392b;
  color: #c0392b;
}
.mono {
  font-family: ui-monospace, monospace;
}
</style>
