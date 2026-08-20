<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ConnectView from './ConnectView.vue'
import { getNativeBridge } from './bridge'

type GateState = 'loading' | 'unconfigured' | 'authorization_required' | 'ready' | 'error'

const bridge = getNativeBridge()
const { t } = useI18n()
const state = ref<GateState>(bridge ? 'loading' : 'ready')
const serverUrl = ref('')
const error = ref('')

function useBootstrapState(next: BootstrapState) {
  state.value = next.state
  serverUrl.value = next.server_url ?? ''
}

async function bootstrap() {
  if (!bridge) return

  state.value = 'loading'
  error.value = ''
  try {
    useBootstrapState(await bridge.Bootstrap())
  } catch {
    state.value = 'error'
    error.value = t('native.connectionError')
  }
}

async function connect(input: ConnectionInput) {
  if (!bridge) return

  const next = await bridge.Connect(input)
  useBootstrapState(next)
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

  <main v-else class="native-gate" role="alert">
    <p>{{ error }}</p>
    <button type="button" @click="bootstrap">{{ t('native.retry') }}</button>
  </main>

</template>
