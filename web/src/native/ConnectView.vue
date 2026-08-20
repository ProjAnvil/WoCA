<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { validateEnterpriseOrigin } from './origin'

const props = defineProps<{
  serverUrl?: string
  authorizationRequired?: boolean
  connect: (input: ConnectionInput) => Promise<unknown>
}>()

const { t } = useI18n()
const serverUrl = ref(props.serverUrl ?? '')
const apiKey = ref('')
const validation = ref('')
const error = ref('')
const pending = ref(false)

watch(
  () => props.serverUrl,
  (value) => {
    serverUrl.value = value ?? ''
  },
)

async function submit() {
  validation.value = ''
  error.value = ''
  if (!serverUrl.value.trim() || !apiKey.value) {
    validation.value = t('native.invalidInput')
    return
  }

  const origin = validateEnterpriseOrigin(serverUrl.value)
  if (!origin) {
    validation.value = t('native.invalidOrigin')
    return
  }

  pending.value = true
  try {
    serverUrl.value = origin
    await props.connect({ server_url: origin, api_key: apiKey.value })
  } catch {
    error.value = t('native.connectionError')
  } finally {
    apiKey.value = ''
    pending.value = false
  }
}
</script>

<template>
  <main class="native-gate">
    <h1>{{ t('native.title') }}</h1>
    <p v-if="authorizationRequired">{{ t('native.authorizationRequired') }}</p>
    <form @submit.prevent="submit">
      <label>
        {{ t('native.urlLabel') }}
        <input v-model="serverUrl" data-testid="url-input" type="url" autocomplete="url" />
      </label>
      <label>
        {{ t('native.apiKeyLabel') }}
        <input
          v-model="apiKey"
          data-testid="api-key-input"
          type="password"
          autocomplete="off"
        />
      </label>
      <p v-if="validation" data-testid="connection-validation" role="alert">{{ validation }}</p>
      <p v-if="error" role="alert">{{ error }}</p>
      <button type="submit" :disabled="pending">{{ t('native.connect') }}</button>
    </form>
  </main>
</template>
