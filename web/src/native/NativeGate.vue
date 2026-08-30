<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ConnectView from './ConnectView.vue'
import { getNativeBridge } from './bridge'
import { setStreamAuth, setStreamBase, setStreamBridge } from '../api/client'
import { hydrateNativePreferences, readPreference } from './preferences'
import { applyStoredLocalePreference } from '../i18n'

type GateState =
  | 'loading'
  | 'unconfigured'
  | 'authorization_required'
  | 'ready'
  | 'fatal_local_data'
  | 'error'

const bridge = getNativeBridge()
const { t } = useI18n()
const state = ref<GateState>(bridge ? 'loading' : 'ready')
const serverUrl = ref('')
const databasePath = ref('')
const embeddedUrl = ref('')
const error = ref('')

// The pre-paint theme script in index.html deliberately skips native mode
// (Web Storage is never touched there), so the gate itself must apply the
// hydrated theme — otherwise the connection and fatal-data screens render in
// the light theme regardless of the stored preference.
function applyNativeTheme() {
  const value = readPreference('woca-theme')
  const dark =
    value === 'dark' ||
    (value !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', dark)
}

async function useBootstrapState(next: BootstrapState) {
  serverUrl.value = next.server_url ?? ''
  databasePath.value = next.database_path ?? ''
  embeddedUrl.value = next.embedded_url ?? ''
  // SSE bypasses the Wails asset proxy (WebView2 buffers proxied
  // streams): stream straight at the bridge origin — edge entries pin
  // their target bridge on the stream request too (v1.6 入口②).
  setStreamBase(next.server_url ?? '')
  setStreamBridge(next.bridge_id ?? '')
  // Remote profiles: the direct SSE connection needs the session Bearer
  // (the asset proxy that normally injects it is bypassed for streams).
  if (next.server_url && bridge) {
    try {
      const cred = await bridge.StreamCredentials(next.server_url)
      setStreamAuth(cred?.scheme ?? '', cred?.token ?? '')
    } catch {
      setStreamAuth('', '')
    }
  } else {
    setStreamAuth('', '')
  }
  if (bridge) {
    if (next.state === 'ready') {
      state.value = 'loading'
      await hydrateNativePreferences(bridge)
      applyStoredLocalePreference()
      applyNativeTheme()
    } else {
      // Gate screens render immediately; theme and locale land as soon as the
      // preference read returns rather than blocking the form behind it.
      void hydrateNativePreferences(bridge)
        .then(() => {
          applyStoredLocalePreference()
          applyNativeTheme()
        })
        .catch(() => {})
    }
  }
  state.value = next.state
}

async function bootstrap() {
  if (!bridge) return

  state.value = 'loading'
  error.value = ''
  try {
    await useBootstrapState(await bridge.Bootstrap())
  } catch {
    state.value = 'error'
    error.value = t('native.connectionError')
  }
}

async function connect(input: ConnectionInput) {
  if (!bridge) return

  const next = await bridge.Connect(input)
  try {
    await useBootstrapState(next)
  } catch {
    state.value = 'error'
    error.value = t('native.connectionError')
    throw new Error('native preference hydration failed')
  }
}

// Entry ① (design §6.6): adopt the embedded bridge — no inputs.
async function connectEmbedded() {
  if (!bridge) return

  try {
    await useBootstrapState(await bridge.ConnectEmbedded())
  } catch {
    state.value = 'error'
    error.value = t('native.connectionError')
  }
}

// The error screen's way back: whatever failed (bootstrap, connect,
// hydration), the user can always return to the connect page instead of
// being trapped in a retry loop.
function backToConnect() {
  error.value = ''
  state.value = serverUrl.value ? 'authorization_required' : 'unconfigured'
}

onMounted(() => {
  void bootstrap()
})
</script>

<template>
  <slot v-if="state === 'ready'" />

  <main v-else-if="state === 'loading'" data-testid="native-loading" class="native-gate">
    <div class="gate-orb gate-orb--indigo" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--violet" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--cyan" aria-hidden="true"></div>
    <section class="gate-card gate-plain gate-center">
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
        <span class="gate-brand-tag">ENTERPRISE</span>
      </div>
      <p class="gate-sub gate-center-row">
        <span class="gate-spinner" aria-hidden="true"></span>
      </p>
    </section>
  </main>

  <ConnectView
    v-else-if="state === 'unconfigured' || state === 'authorization_required'"
    :server-url="serverUrl"
    :authorization-required="state === 'authorization_required'"
    :embedded-url="embeddedUrl"
    :connect="connect"
    @connect-embedded="connectEmbedded"
  />

  <main
    v-else-if="state === 'fatal_local_data'"
    data-testid="fatal-local-data"
    class="native-gate"
    role="alert"
  >
    <div class="gate-orb gate-orb--indigo" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--violet" aria-hidden="true"></div>
    <section class="gate-card gate-plain">
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
        <span class="gate-brand-tag">ENTERPRISE</span>
      </div>
      <h1>{{ t('native.localDataFatalTitle') }}</h1>
      <p>{{ t('native.localDataFatalDescription') }}</p>
      <p>{{ t('native.localDataFatalRecovery') }}</p>
      <p>
        <span>{{ t('native.databasePathLabel') }}</span>
        <code>{{ databasePath || t('native.databasePathUnavailable') }}</code>
      </p>
    </section>
  </main>

  <main v-else class="native-gate" role="alert">
    <div class="gate-orb gate-orb--indigo" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--violet" aria-hidden="true"></div>
    <section class="gate-card gate-plain">
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
        <span class="gate-brand-tag">ENTERPRISE</span>
      </div>
      <p>{{ error }}</p>
      <div class="gate-error-actions">
        <button type="button" class="gate-ghost" @click="bootstrap">{{ t('native.retry') }}</button>
        <button type="button" class="gate-ghost" @click="backToConnect">
          {{ t('native.backToConnect') }}
        </button>
      </div>
    </section>
  </main>
</template>
