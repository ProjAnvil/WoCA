<script setup lang="ts">
import { toErrorMessage } from '../../lib/errors'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { CloudUpload, KeyRound, Loader2, ShieldOff, Unplug } from 'lucide-vue-next'
import {
  getCloudAccess,
  resetCloudAccess,
  updateCloudAccess,
  type CloudAccessStatus,
} from '../../api/client'

// The bridge's cloud-access section (product design §6.5): bridge-local
// infrastructure configuration — the edge URL, the one-time enrollment
// token, and the registration state. Deliberately editable where the
// managed domains are read-only: this layer belongs to the bridge, not
// the config plane (§3.3's two-layer separation).
const { t } = useI18n()

const st = ref<CloudAccessStatus | null>(null)
const url = ref('')
const token = ref('')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const note = ref('')
const resetting = ref(false)

async function reload() {
  loading.value = true
  error.value = ''
  try {
    st.value = await getCloudAccess()
    url.value = st.value.url
    if (st.value.url_source === 'env') note.value = t('settings.cloud.envPinned')
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(reload)

async function save() {
  if (saving.value) return
  saving.value = true
  error.value = ''
  note.value = ''
  try {
    st.value = await updateCloudAccess(url.value.trim(), token.value.trim())
    url.value = st.value.url
    token.value = ''
    note.value = t('settings.cloud.saved')
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    saving.value = false
  }
}

// 注销: wipe the local machine identity. Server-side revocation stays
// the administrator's action (console), this only clears this bridge.
async function deregister() {
  if (resetting.value || !confirm(t('settings.cloud.resetConfirm'))) return
  resetting.value = true
  error.value = ''
  try {
    st.value = await resetCloudAccess()
    url.value = ''
    token.value = ''
    note.value = t('settings.cloud.resetDone')
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    resetting.value = false
  }
}
</script>

<template>
  <section class="cloud-section">
    <div v-if="loading" class="cloud-loading"><Loader2 class="spin" :size="15" /></div>
    <template v-else-if="st">
      <div class="cloud-grid">
        <label>
          {{ t('settings.cloud.edgeUrl') }}
          <input
            v-model="url"
            data-testid="cloud-url"
            type="url"
            :placeholder="t('settings.cloud.edgeUrlPlaceholder')"
            :disabled="st.url_source === 'env'"
            spellcheck="false"
          />
        </label>
        <label>
          {{ t('settings.cloud.enrollToken') }}
          <input
            v-model="token"
            data-testid="cloud-token"
            type="password"
            autocomplete="off"
            :placeholder="st.token_set ? t('settings.cloud.tokenConsumed') : t('settings.cloud.tokenPlaceholder')"
          />
        </label>
      </div>
      <p v-if="st.url_source === 'env'" class="cloud-hint">{{ t('settings.cloud.envPinned') }}</p>
      <p v-else class="cloud-hint">{{ t('settings.cloud.urlHint') }}</p>
      <p v-if="st.token_set && st.token_source === 'env'" class="cloud-hint">
        {{ t('settings.cloud.tokenEnvPinned') }}
      </p>

      <div class="cloud-actions">
        <button class="btn btn-primary" data-testid="cloud-save" :disabled="saving" @click="save">
          <Loader2 v-if="saving" class="spin" :size="14" />
          {{ t('settings.cloud.save') }}
        </button>
        <button
          v-if="st.enrolled"
          class="btn btn-danger"
          data-testid="cloud-reset"
          :disabled="resetting"
          @click="deregister"
        >
          <ShieldOff v-if="false" :size="14" />
          <Unplug :size="14" />
          {{ t('settings.cloud.reset') }}
        </button>
      </div>

      <dl class="cloud-state">
        <dt>{{ t('settings.cloud.state') }}</dt>
        <dd>
          <span class="lamp" :class="{ ok: st.enrolled && !st.stale, warn: st.enrolled && st.stale }" />
          {{
            !st.enrolled
              ? t('settings.cloud.stateUnregistered')
              : st.stale
                ? t('settings.cloud.stateStale')
                : t('settings.cloud.stateRegistered')
          }}
        </dd>
        <template v-if="st.enrolled">
          <dt>{{ t('settings.cloud.bridgeId') }}</dt>
          <dd>{{ st.bridge_id }}</dd>
          <dt>{{ t('settings.cloud.configVersion') }}</dt>
          <dd>{{ st.last_version || '—' }}</dd>
          <dt>{{ t('settings.cloud.lastSync') }}</dt>
          <dd>{{ st.last_pull || '—' }}</dd>
          <dt>{{ t('settings.cloud.spki') }}</dt>
          <dd>
            {{
              st.spki_pin
                ? `${st.spki_pin.slice(0, 16)}…`
                : t('common.no')
            }}
          </dd>
          <dt>{{ t('settings.cloud.tunnel') }}</dt>
          <dd>
            {{
              st.tunnel.serving
                ? t('settings.cloud.tunnelServing', { n: st.tunnel.served })
                : t('settings.cloud.tunnelOff')
            }}
          </dd>
        </template>
      </dl>
    </template>
    <p v-if="error" class="cloud-error" role="alert">{{ error }}</p>
    <p v-if="note" class="cloud-note">{{ note }}</p>
  </section>
</template>

<style scoped>
.cloud-section {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.cloud-loading {
  display: flex;
  justify-content: center;
  padding: 12px;
  color: var(--muted, #6e7681);
}
.cloud-grid {
  display: grid;
  gap: 10px;
}
.cloud-grid label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12.5px;
}
.cloud-grid input {
  padding: 8px 10px;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 8px;
  background: var(--bg, #14161a);
  color: inherit;
  font: inherit;
}
.cloud-grid input:disabled {
  opacity: 0.55;
}
.cloud-hint {
  margin: 0;
  font-size: 11.5px;
  color: var(--muted, #6e7681);
}
.cloud-actions {
  display: flex;
  gap: 8px;
}
.cloud-state {
  display: grid;
  grid-template-columns: 110px 1fr;
  gap: 4px 10px;
  margin: 0;
  padding: 10px 12px;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 8px;
  font-size: 12.5px;
}
.cloud-state dt {
  opacity: 0.65;
}
.cloud-state dd {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 6px;
  font-family: ui-monospace, monospace;
  overflow-wrap: anywhere;
}
.lamp {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted, #6e7681);
  flex: none;
}
.lamp.ok {
  background: var(--ok, #3fb950);
}
.lamp.warn {
  background: var(--warn, #d29922);
}
.cloud-error {
  margin: 0;
  color: #e5534b;
  font-size: 12.5px;
}
.cloud-note {
  margin: 0;
  color: var(--ok, #3fb950);
  font-size: 12.5px;
}
.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-color: #a33;
  color: #e88;
}
</style>
