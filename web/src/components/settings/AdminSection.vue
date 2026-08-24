<script setup lang="ts">
import { toErrorMessage } from '../../lib/errors'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, ScrollText, Users } from 'lucide-vue-next'
import { listAudit, listBridgeSessions, type AuditRow, type BridgeSession } from '../../api/client'

// The bridge admin page (product design §5.1 管理页, v1.7): a READ-ONLY
// operations view — the live-session inventory and the local audit trail.
// Key management is not here: organization credentials are issued by the
// tenant ops console on the private core, never on the bridge.
const { t } = useI18n()

const sessions = ref<BridgeSession[]>([])
const audit = ref<AuditRow[]>([])
const loading = ref(false)
const error = ref('')

async function reload() {
  loading.value = true
  error.value = ''
  try {
    ;[sessions.value, audit.value] = await Promise.all([listBridgeSessions(), listAudit(50)])
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(reload)

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}
</script>

<template>
  <section class="setting-section">
    <div class="setting-section-title">{{ t('admin.title') }}</div>
    <p class="admin-hint">{{ t('admin.readonlyHint') }}</p>
    <p v-if="error" class="admin-error">{{ error }}</p>

    <div v-if="loading" class="admin-loading"><Loader2 class="spin" :size="16" /></div>
    <template v-else>
      <div class="admin-card">
        <div class="admin-card-title"><Users :size="14" /> {{ t('admin.sessionsTitle') }}</div>
        <table class="admin-table">
          <thead>
            <tr><th>{{ t('admin.remark') }}</th><th>{{ t('admin.role') }}</th><th>{{ t('admin.lastSeen') }}</th><th>{{ t('admin.expires') }}</th></tr>
          </thead>
          <tbody>
            <tr v-for="s in sessions" :key="s.key_prefix">
              <td>{{ s.remark || s.key_prefix }}</td>
              <td>{{ s.role }}</td>
              <td>{{ fmtTime(s.last_seen) }}</td>
              <td>{{ fmtTime(s.expires_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="admin-card">
        <div class="admin-card-title"><ScrollText :size="14" /> {{ t('admin.auditTitle') }}</div>
        <table class="admin-table">
          <thead>
            <tr><th>{{ t('admin.when') }}</th><th>{{ t('admin.who') }}</th><th>{{ t('admin.action') }}</th><th>{{ t('admin.detail') }}</th></tr>
          </thead>
          <tbody>
            <tr v-for="(e, i) in audit" :key="i">
              <td>{{ fmtTime(e.at) }}</td>
              <td>{{ e.actor }}</td>
              <td>{{ e.action }}</td>
              <td class="admin-detail">{{ e.detail }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

<style scoped>
.admin-card {
  border: 1px solid var(--border, #2a2e37);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 12px;
}
.admin-card-title {
  display: flex;
  align-items: center;
  gap: 6px;
  font-weight: 600;
  margin-bottom: 8px;
  font-size: 13px;
}
.admin-hint {
  margin: 0 0 10px;
  font-size: 12.5px;
  opacity: 0.75;
}
.admin-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
}
.admin-table th {
  text-align: left;
  opacity: 0.6;
  font-weight: 500;
  padding: 3px 8px 6px 0;
}
.admin-table td {
  padding: 4px 8px 4px 0;
  border-top: 1px solid var(--border, #22252d);
}
.admin-detail {
  font-family: ui-monospace, monospace;
  overflow-wrap: anywhere;
  max-width: 220px;
}
.admin-error { color: #e5534b; font-size: 12.5px; }
.admin-loading { display: flex; justify-content: center; padding: 16px; }</style>
