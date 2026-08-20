import { defineStore } from 'pinia'
import { useToggle } from '@vueuse/core'
import { computed, ref } from 'vue'
import { getSettings, updateSettings } from '../api/client'
import i18n, { applyLocale, LOCALE_STORAGE_KEY, resolveLocale, type LocaleOption } from '../i18n'
import type { AppSettings, AppSettingsUpdate } from '../types'
import { useAgentStore } from './agent'
import { usePreference, useThemePreference } from '../native/preferences'

// Settings + appearance state. Browser preferences retain their localStorage
// keys; the native gate hydrates the same values from SQLite before this store
// is created. Provider/storage settings remain server-backed via /api/settings.
export const useSettingsStore = defineStore('settings', () => {
  const isDark = useThemePreference()
  const toggleDark = useToggle(isDark)

  const panelOpen = ref(false)

  const localePref = usePreference<LocaleOption>(LOCALE_STORAGE_KEY, 'auto')
  const locale = computed(() => resolveLocale(localePref.value))
  function setLocale(pref: LocaleOption) {
    localePref.value = pref
    applyLocale(pref)
  }

  // Client-side preferences (cards in the settings panel). The refs stay
  // assignable through the store while the adapter chooses browser or native
  // persistence.
  const autoCollapseTools = usePreference('woca.pref.autoCollapseTools', true)
  const markdownPreview = usePreference('woca.pref.markdownPreview', true)
  const model = usePreference('woca.pref.model', 'claude-sonnet-4-5')
  const permissionMode = usePreference<'default' | 'acceptEdits' | 'bypassPermissions' | 'readOnly' | 'plan'>(
    'woca.pref.permissionMode',
    'default',
  )

  // --- server-backed settings ---
  const server = ref<AppSettings | null>(null)
  const serverError = ref('')
  const saving = ref(false)
  const savedMessage = ref('')
  const restartRequired = ref(false)

  // Local editable copies for the form (password inputs stay empty unless
  // the user types a new value).
  type ProviderName = 'anthropic' | 'openai' | 'glm' | 'deepseek'
  const provider = ref<ProviderName>('anthropic')
  const serverModel = ref('')
  const baseURLs = ref<Record<ProviderName, string>>({
    anthropic: '',
    openai: '',
    glm: '',
    deepseek: '',
  })
  const apiKeys = ref<Record<ProviderName, string>>({
    anthropic: '',
    openai: '',
    glm: '',
    deepseek: '',
  })
  const anthropicBaseURL = computed({
    get: () => baseURLs.value.anthropic,
    set: (v) => (baseURLs.value.anthropic = v),
  })
  const openaiBaseURL = computed({
    get: () => baseURLs.value.openai,
    set: (v) => (baseURLs.value.openai = v),
  })
  const anthropicAPIKey = computed({
    get: () => apiKeys.value.anthropic,
    set: (v) => (apiKeys.value.anthropic = v),
  })
  const openaiAPIKey = computed({
    get: () => apiKeys.value.openai,
    set: (v) => (apiKeys.value.openai = v),
  })
  const checkpointBackend = ref('sqlite')
  const memoryBackend = ref('sqlite')
  const postgresURL = ref('')
  const debugLog = ref(false)

  // ladyM factual-memory engine (live-applied server-side on save).
  const memoryEngine = ref('ladym')
  const ladymEmbedding = ref('inherit')
  const ladymEmbeddingModel = ref('')
  const ladymEmbeddingBaseURL = ref('')
  const ladymEmbeddingAPIKey = ref('')
  const ladymLLM = ref('inherit')
  const ladymAutoRetain = ref(true)
  const ladymTopK = ref(6)
  const ladymDBPath = ref('')
  const ladymSystem2 = ref(false)
  const ladymSystem2Interval = ref(300)

  // Unified credential fields: one Base URL + one API Key input that target
  // the selected provider's stored fields (the user picks a provider, not a
  // per-provider form). Drafts are kept per provider so switching back and
  // forth does not lose typed values.
  //
  // Implicit security contract: an EMPTY key field means "unchanged", never
  // "clear". The password inputs always render empty (even when a key is
  // set — see apiKeySet), so saveServer only forwards non-empty secrets and
  // loadServer resets the drafts to ''. Turning an empty string into an
  // explicit clear would let a casual "save" destroy an encrypted key the
  // form never even displayed.
  const baseURL = computed({
    get: () => baseURLs.value[provider.value],
    set: (v: string) => (baseURLs.value[provider.value] = v),
  })
  const apiKey = computed({
    get: () => apiKeys.value[provider.value],
    set: (v: string) => (apiKeys.value[provider.value] = v),
  })
  const apiKeySet = computed(() => {
    switch (provider.value) {
      case 'openai':
        return Boolean(server.value?.openai_api_key_set)
      case 'glm':
        return Boolean(server.value?.glm_api_key_set)
      case 'deepseek':
        return Boolean(server.value?.deepseek_api_key_set)
      default:
        return Boolean(server.value?.anthropic_api_key_set)
    }
  })
  const ladymEmbeddingKeySet = computed(() => Boolean(server.value?.ladym_embedding_api_key_set))

  async function loadServer() {
    serverError.value = ''
    try {
      const s = await getSettings()
      server.value = s
      provider.value = (s.provider as ProviderName) ?? 'anthropic'
      serverModel.value = s.model
      baseURLs.value = {
        anthropic: s.anthropic_base_url,
        openai: s.openai_base_url,
        glm: s.glm_base_url,
        deepseek: s.deepseek_base_url,
      }
      checkpointBackend.value = s.checkpoint_backend || 'sqlite'
      memoryBackend.value = s.memory_backend || 'sqlite'
      debugLog.value = Boolean(s.debug_log)
      memoryEngine.value = s.memory_engine || 'ladym'
      ladymEmbedding.value = s.ladym_embedding || 'inherit'
      ladymEmbeddingModel.value = s.ladym_embedding_model ?? ''
      ladymEmbeddingBaseURL.value = s.ladym_embedding_base_url ?? ''
      ladymLLM.value = s.ladym_llm || 'inherit'
      ladymAutoRetain.value = s.ladym_autoretain !== false
      ladymTopK.value = s.ladym_topk || 6
      ladymDBPath.value = s.ladym_db_path ?? ''
      ladymSystem2.value = Boolean(s.ladym_system2)
      ladymSystem2Interval.value = s.ladym_system2_interval || 300
      ladymEmbeddingAPIKey.value = ''
      apiKeys.value = { anthropic: '', openai: '', glm: '', deepseek: '' }
      postgresURL.value = ''
    } catch (e) {
      serverError.value = e instanceof Error ? e.message : String(e)
    }
  }

  async function saveServer() {
    saving.value = true
    savedMessage.value = ''
    serverError.value = ''
    const patch: AppSettingsUpdate = {
      provider: provider.value,
      model: serverModel.value,
      anthropic_base_url: baseURLs.value.anthropic,
      openai_base_url: baseURLs.value.openai,
      glm_base_url: baseURLs.value.glm,
      deepseek_base_url: baseURLs.value.deepseek,
      checkpoint_backend: checkpointBackend.value,
      memory_backend: memoryBackend.value,
      memory_engine: memoryEngine.value,
      ladym_embedding: ladymEmbedding.value,
      ladym_embedding_model: ladymEmbeddingModel.value,
      ladym_embedding_base_url: ladymEmbeddingBaseURL.value,
      ladym_llm: ladymLLM.value,
      ladym_autoretain: ladymAutoRetain.value,
      // An emptied input yields ''/NaN, which the server would reject with a
      // 400 — fall back to the default of 6 for invalid values.
      ladym_topk: Number.isFinite(ladymTopK.value) && ladymTopK.value >= 1 ? ladymTopK.value : 6,
      ladym_system2_interval:
        Number.isFinite(ladymSystem2Interval.value) && ladymSystem2Interval.value >= 60
          ? ladymSystem2Interval.value
          : 300,
      ladym_db_path: ladymDBPath.value,
      ladym_system2: ladymSystem2.value,
    }
    // Secrets: only send what the user actually typed. An empty draft means
    // "keep the stored key" — the server treats absent fields as unchanged,
    // and forwarding '' would wipe an encrypted key the form never showed.
    if (apiKeys.value.anthropic) patch.anthropic_api_key = apiKeys.value.anthropic
    if (apiKeys.value.openai) patch.openai_api_key = apiKeys.value.openai
    if (apiKeys.value.glm) patch.glm_api_key = apiKeys.value.glm
    if (apiKeys.value.deepseek) patch.deepseek_api_key = apiKeys.value.deepseek
    if (postgresURL.value) patch.postgres_url = postgresURL.value
    if (ladymEmbeddingAPIKey.value) patch.ladym_embedding_api_key = ladymEmbeddingAPIKey.value
    try {
      const res = await updateSettings(patch)
      restartRequired.value = res.restart_required
      savedMessage.value = res.restart_required
        ? i18n.global.t('settings.store.savedRestart')
        : res.apply_error
          ? i18n.global.t('settings.store.savedApplyError', { error: res.apply_error })
          : i18n.global.t('settings.store.saved')
      if (serverModel.value) model.value = serverModel.value
      await loadServer()
      // Provider/model changes may flip the thinking drawer's support state.
      const agentStore = useAgentStore()
      void agentStore.load()
    } catch (e) {
      serverError.value = e instanceof Error ? e.message : String(e)
    } finally {
      saving.value = false
    }
  }

  async function toggleDebugLog() {
    const next = !debugLog.value
    try {
      await updateSettings({ debug_log: next })
      debugLog.value = next
      await loadServer()
    } catch (e) {
      serverError.value = e instanceof Error ? e.message : String(e)
    }
  }

  function openPanel() {
    panelOpen.value = true
    void loadServer()
  }
  function closePanel() {
    panelOpen.value = false
  }

  return {
    isDark,
    toggleDark,
    panelOpen,
    localePref,
    locale,
    setLocale,
    autoCollapseTools,
    markdownPreview,
    model,
    permissionMode,
    server,
    serverError,
    saving,
    savedMessage,
    restartRequired,
    provider,
    serverModel,
    baseURL,
    apiKey,
    apiKeySet,
    anthropicBaseURL,
    openaiBaseURL,
    anthropicAPIKey,
    openaiAPIKey,
    checkpointBackend,
    memoryBackend,
    postgresURL,
    debugLog,
    memoryEngine,
    ladymEmbedding,
    ladymEmbeddingModel,
    ladymEmbeddingBaseURL,
    ladymEmbeddingAPIKey,
    ladymEmbeddingKeySet,
    ladymLLM,
    ladymAutoRetain,
    ladymTopK,
    ladymDBPath,
    ladymSystem2,
    ladymSystem2Interval,
    logDir: computed(() => server.value?.log_dir ?? ''),
    toggleDebugLog,
    loadServer,
    saveServer,
    openPanel,
    closePanel,
  }
})
