<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ConnectView from './ConnectView.vue'
import { getNativeBridge } from './bridge'
import { hydrateNativePreferences } from './preferences'
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

async function useBootstrapState(next: BootstrapState) {
  serverUrl.value = next.server_url ?? ''
  databasePath.value = next.database_path ?? ''
  if (next.state === 'ready' && bridge) {
    state.value = 'loading'
    await hydrateNativePreferences(bridge)
    applyStoredLocalePreference()
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
