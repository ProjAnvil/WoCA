<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Globe, KeyRound, LoaderCircle, ShieldCheck, TriangleAlert } from 'lucide-vue-next'
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
        <span class="gate-brand-tag">ENTERPRISE</span>
      </div>

      <h1>{{ t('native.title') }}</h1>
      <p class="gate-sub">{{ t('native.subtitle') }}</p>

      <p v-if="authorizationRequired" class="gate-notice">
        <TriangleAlert :size="15" aria-hidden="true" />
        {{ t('native.authorizationRequired') }}
      </p>

      <form class="gate-form" @submit.prevent="submit">
        <label>
          {{ t('native.urlLabel') }}
          <span class="gate-field">
            <Globe :size="16" aria-hidden="true" />
            <input
              v-model="serverUrl"
              data-testid="url-input"
              type="url"
              autocomplete="url"
              :placeholder="t('native.urlPlaceholder')"
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
