<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Globe,
  HardDrive,
  KeyRound,
  LoaderCircle,
  MonitorSmartphone,
  ShieldCheck,
  TriangleAlert,
} from 'lucide-vue-next'
import { getNativeBridge } from './bridge'
import { validateEnterpriseOrigin } from './origin'

const props = defineProps<{
  serverUrl?: string
  authorizationRequired?: boolean
  /** Entry ① (wails only): the embedded bridge's base URL. */
  embeddedUrl?: string
  connect: (input: ConnectionInput) => Promise<unknown>
}>()

const emit = defineEmits<{ 'connect-embedded': [] }>()

const { t } = useI18n()
type Tab = 'private' | 'direct'
const tab = ref<Tab>('private')
const serverUrl = ref(props.serverUrl ?? '')
const apiKey = ref('')
const validation = ref('')
const error = ref('')
const pending = ref(false)
// Bridge selection step (entry ②): the edge fronts several bridges.
const step = ref<'form' | 'bridges'>('form')
const bridges = ref<EdgeBridge[]>([])

const urlLabel = computed(() => (tab.value === 'private' ? t('native.urlLabelPrivate') : t('native.urlLabelDirect')))
const urlPlaceholder = computed(() =>
  tab.value === 'private' ? t('native.urlPlaceholderPrivate') : t('native.urlPlaceholderDirect'),
)

watch(
  () => props.serverUrl,
  (value) => {
    serverUrl.value = value ?? ''
  },
)
watch(tab, () => {
  validation.value = ''
  error.value = ''
})

async function useEmbedded() {
  error.value = ''
  pending.value = true
  try {
    emit('connect-embedded')
  } finally {
    pending.value = false
  }
}

async function submit() {
  validation.value = ''
  error.value = ''
  if (!serverUrl.value.trim() || !apiKey.value) {
    validation.value = t('native.invalidInput')
    return
  }
  // Direct link form (design §6.6 LIST alternative):
  // https://edge.example.com/?bridge=<id> pins the target up front —
  // strip the query before origin validation (which rejects queries).
  let directBridge = ''
  const raw = serverUrl.value.trim()
  const qm = raw.indexOf('?')
  if (qm >= 0) {
    const m = /(?:^|&)(?:bridge|bridge_id)=([^&\s]+)/.exec(raw.slice(qm + 1))
    // Only a bridge-pinning query is stripped and honored; any other
    // query stays and fails origin validation (unknown query = reject).
    if (m) {
      directBridge = decodeURIComponent(m[1])
      serverUrl.value = raw.slice(0, qm)
    }
  }
  const origin = validateEnterpriseOrigin(serverUrl.value)
  if (!origin) {
    validation.value = t('native.invalidOrigin')
    return
  }
  pending.value = true
  keepKey = false
  try {
    serverUrl.value = origin
    if (directBridge) {
      await doConnect(directBridge)
      return
    }
    // Entry ② semantics: an edge may front several bridges — ask for the
    // pre-auth inventory and let the user pick. Unavailable inventories
    // (older binaries, non-native) fall through to a plain connect; a
    // single bridge needs no choice (the edge auto-routes).
    if (tab.value === 'private') {
      const native = getNativeBridge()
      if (native?.EdgeBridges) {
        try {
          const list = await native.EdgeBridges(origin)
          if (list.length === 0) {
            error.value = t('native.noBridges')
            return
          }
          if (list.length > 1) {
            bridges.value = list
            step.value = 'bridges'
            keepKey = true // the pick needs it; pickBridge clears it
            return
          }
        } catch {
          // Inventory unreachable: let the connection attempt surface
          // the real error.
        }
      }
    }
    await doConnect()
  } catch {
    error.value = t('native.connectionError')
  } finally {
    if (!keepKey) apiKey.value = ''
    pending.value = false
  }
}

// keepKey marks the selection-step transition: the API key must survive
// until the user picks the target bridge (pickBridge clears it).
let keepKey = false

async function doConnect(bridgeId?: string) {
  const input: ConnectionInput = { server_url: serverUrl.value, api_key: apiKey.value }
  if (bridgeId) input.bridge_id = bridgeId
  await props.connect(input)
}

async function pickBridge(id: string) {
  pending.value = true
  try {
    await doConnect(id)
  } catch {
    error.value = t('native.connectionError')
  } finally {
    apiKey.value = ''
    pending.value = false
  }
}

function backToForm() {
  step.value = 'form'
  bridges.value = []
  error.value = ''
}
</script>

<template>
  <main class="native-gate">
    <div class="gate-orb gate-orb--indigo" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--violet" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--cyan" aria-hidden="true"></div>

    <section class="gate-card">
      <div class="gate-brand">
        <div class="gate-logo" aria-hidden="true">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
            <path
              d="M4 7l2.4 10h2.2L11 9.6 13.4 17h2.2L18 7h-2.1l-1.6 7.2L12 7h-2L7.7 14.2 6.1 7H4z"
              fill="currentColor"
            />
          </svg>
        </div>
        <span class="gate-wordmark">WOCA</span>
        <span class="gate-brand-tag">CONNECT</span>
      </div>

      <h1>{{ t('native.title') }}</h1>
      <p class="gate-sub">{{ t('native.subtitle') }}</p>

      <p v-if="authorizationRequired" class="gate-notice">
        <TriangleAlert :size="15" aria-hidden="true" />
        {{ t('native.authorizationRequired') }}
      </p>

      <!-- Entry ①: the embedded bridge (wails only) — auto-launched,
           loopback-trusted, zero inputs (design §6.6). -->
      <div v-if="embeddedUrl" data-testid="embedded-entry" class="gate-embedded">
        <div class="gate-embedded-text">
          <strong>{{ t('native.embeddedTitle') }}</strong>
          <span>{{ t('native.embeddedHint') }}</span>
        </div>
        <button type="button" data-testid="embedded-connect" class="gate-embedded-btn" @click="useEmbedded">
          <HardDrive :size="15" aria-hidden="true" />
          {{ t('native.embeddedUse') }}
        </button>
      </div>

      <!-- Bridge selection (entry ②): the edge fronts several bridges. -->
      <div v-if="step === 'bridges'" data-testid="bridge-step" class="gate-form">
        <p class="gate-step-title">{{ t('native.chooseBridge') }}</p>
        <p class="gate-sub">{{ t('native.chooseBridgeHint') }}</p>
        <ul class="gate-bridges">
          <li v-for="b in bridges" :key="b.bridge_id">
            <button type="button" class="gate-bridge" @click="pickBridge(b.bridge_id)">
              <MonitorSmartphone :size="16" aria-hidden="true" />
              <code>{{ b.bridge_id }}</code>
              <span class="gate-bridge-live" :class="{ live: b.live }">
                {{ b.live ? '● online' : '● offline' }}
              </span>
            </button>
          </li>
        </ul>
        <p v-if="error" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ error }}
        </p>
        <button type="button" class="gate-back" @click="backToForm">{{ t('native.backToForm') }}</button>
      </div>

      <!-- Entries ②/③: one credential shape (URL + key) behind two
           semantic tabs — the same profile mechanism underneath. -->
      <form v-else class="gate-form" @submit.prevent="submit">
        <div class="gate-tabs" role="tablist">
          <button
            type="button"
            role="tab"
            data-testid="tab-private"
            :aria-selected="tab === 'private'"
            :class="{ active: tab === 'private' }"
            @click="tab = 'private'"
          >
            {{ t('native.tabPrivate') }}
          </button>
          <button
            type="button"
            role="tab"
            data-testid="tab-direct"
            :aria-selected="tab === 'direct'"
            :class="{ active: tab === 'direct' }"
            @click="tab = 'direct'"
          >
            {{ t('native.tabDirect') }}
          </button>
        </div>
        <label>
          {{ urlLabel }}
          <span class="gate-field">
            <Globe :size="16" aria-hidden="true" />
            <input
              v-model="serverUrl"
              data-testid="url-input"
              type="url"
              autocomplete="url"
              :placeholder="urlPlaceholder"
              spellcheck="false"
            />
          </span>
        </label>
        <label>
          {{ t('native.apiKeyLabel') }}
          <span class="gate-field">
            <KeyRound :size="16" aria-hidden="true" />
            <input
              v-model="apiKey"
              data-testid="api-key-input"
              type="password"
              autocomplete="off"
              :placeholder="t('native.apiKeyPlaceholder')"
            />
          </span>
        </label>
        <p v-if="validation" data-testid="connection-validation" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ validation }}
        </p>
        <p v-if="error" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ error }}
        </p>
        <button type="submit" class="gate-submit" :disabled="pending">
          <LoaderCircle v-if="pending" class="gate-spinner" :size="15" aria-hidden="true" />
          {{ pending ? t('native.connecting') : t('native.connect') }}
        </button>
      </form>

      <p class="gate-foot">
        <ShieldCheck aria-hidden="true" />
        {{ t('native.keyHint') }}
      </p>
    </section>
  </main>
</template>

<style scoped>
.gate-embedded {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 10px;
  background: rgba(79, 124, 255, 0.06);
}
.gate-embedded-text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 12.5px;
}
.gate-embedded-text span {
  color: #9aa3b2;
  font-size: 11.5px;
}
.gate-embedded-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 7px 12px;
  border-radius: 8px;
  border: 1px solid var(--accent, #4f7cff);
  background: transparent;
  color: var(--accent, #4f7cff);
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
  white-space: nowrap;
}
.gate-embedded-btn:hover {
  background: rgba(79, 124, 255, 0.12);
}
.gate-tabs {
  display: flex;
  gap: 6px;
  padding: 4px;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 10px;
}
.gate-tabs button {
  flex: 1;
  padding: 7px 0;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: #9aa3b2;
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
}
.gate-tabs button.active {
  background: rgba(79, 124, 255, 0.16);
  color: var(--accent, #7fa5ff);
}
.gate-step-title {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}
.gate-bridges {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.gate-bridge {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 10px 12px;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 10px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  font-size: 13px;
}
.gate-bridge:hover {
  border-color: var(--accent, #4f7cff);
}
.gate-bridge code {
  flex: 1;
  text-align: left;
  font-size: 12.5px;
}
.gate-bridge-live {
  color: #8b93a1;
  font-size: 11.5px;
}
.gate-bridge-live.live {
  color: #2fbf71;
}
.gate-back {
  align-self: center;
  border: none;
  background: none;
  color: var(--accent, #7fa5ff);
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
  padding: 0;
}
</style>
