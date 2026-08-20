<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ConnectView from './ConnectView.vue'
import { getNativeBridge } from './bridge'
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

onMounted(() => {
  void bootstrap()
})
</script>

<template>
  <slot v-if="state === 'ready'" />

  <main v-else-if="state === 'loading'" data-testid="native-loading" class="native-gate">
    Loading…
  </main>

  <ConnectView
    v-else-if="state === 'unconfigured' || state === 'authorization_required'"
    :server-url="serverUrl"
    :authorization-required="state === 'authorization_required'"
    :connect="connect"
  />

  <main
    v-else-if="state === 'fatal_local_data'"
    data-testid="fatal-local-data"
    class="native-gate"
    role="alert"
  >
    <h1>{{ t('native.localDataFatalTitle') }}</h1>
    <p>{{ t('native.localDataFatalDescription') }}</p>
    <p>{{ t('native.localDataFatalRecovery') }}</p>
    <p>
      <span>{{ t('native.databasePathLabel') }}</span>
      <code>{{ databasePath || t('native.databasePathUnavailable') }}</code>
    </p>
  </main>

  <main v-else class="native-gate" role="alert">
    <p>{{ error }}</p>
    <button type="button" @click="bootstrap">{{ t('native.retry') }}</button>
  </main>

</template>
