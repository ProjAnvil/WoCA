// Server-backed settings: the /api/settings snapshot, the settings form's
// editable drafts, the managed-domain metadata, and the private-cloud
// model catalog. Split from the client-preferences store
// (stores/settings.ts) so each has one reason to change.
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  applyModel as applyModelPreset,
  getSettings,
  listModels,
  updateSettings,
  type ModelPreset,
} from '../api/client'
import i18n from '../i18n'
import { toErrorMessage } from '../lib/errors'
import type {
  AppSettings,
  AppSettingsUpdate,
  CheckpointBackend,
  MemoryBackend,
  ProviderName,
} from '../types'
import { useAgentStore } from './agent'
import { useSettingsStore } from './settings'

export const useServerSettingsStore = defineStore('serverSettings', () => {
  // --- managed-settings metadata (product design §3.1) ---
  // Keys locked by the config plane; the matching fields render read-only
  // with a lock icon and a Managed banner.
  const managedFields = computed<string[]>(() => server.value?.managed_fields ?? [])
  const isManaged = (key: string) => managedFields.value.includes(key)
  const modelWhitelist = computed<string[]>(() =>
    (server.value?.model_whitelist ?? '')
      .split(',')
      .map((m) => m.trim())
      .filter(Boolean),
  )

  // --- model catalog (private-cloud form, product ruling: the bridge
  // pulls the operator-authored preset list; in the managed form this is
  // the ONLY selectable source — empty catalog means the deployment
  // offers no models and the composer must refuse to send) ---
  const models = ref<ModelPreset[]>([])
  const modelsLoaded = ref(false)
  const applyingModel = ref(false)

  async function ensureModels() {
    if (modelsLoaded.value) return
    try {
      models.value = await listModels()
    } catch {
      models.value = []
    } finally {
      modelsLoaded.value = true
    }
  }

  async function applyModel(id: string) {
    if (applyingModel.value) return
    applyingModel.value = true
    serverError.value = ''
    try {
      const out = await applyModelPreset(id)
      if (out.apply_error) serverError.value = out.apply_error
    } catch (e) {
      serverError.value = toErrorMessage(e)
    } finally {
      applyingModel.value = false
    }
  }

  const noModelsManaged = computed(
    () => managedFields.value.length > 0 && modelsLoaded.value && models.value.length === 0,
  )

  // --- server snapshot ---
  const server = ref<AppSettings | null>(null)
  const serverError = ref('')
  const saving = ref(false)
  const savedMessage = ref('')
  const restartRequired = ref(false)

  // Local editable copies for the form (password inputs stay empty unless
  // the user types a new value).
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
  const checkpointBackend = ref<CheckpointBackend>('sqlite')
  const memoryBackend = ref<MemoryBackend>('sqlite')
  const postgresURL = ref('')
  const debugLog = ref(false)
  const langfuseEnabled = ref(false)
  const langfuseBaseURL = ref('')
  const langfusePublicKey = ref('')
  const langfuseSecret = ref('')
  const msgPlaneMode = ref<'pg' | 'pg+nats'>('pg')
  const msgPlaneNATSURL = ref('')
  const msgPlaneSubject = ref('runs.dispatch')

  // ladyM factual-memory engine (live-applied server-side on save).
  const memoryEngine = ref<'ladym' | 'remote' | 'none'>('ladym')
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
  const langfuseConsoleManaged = computed(() => Boolean(server.value?.langfuse_console_managed))
  const msgPlaneAvailable = computed(() => Boolean(server.value?.msg_plane_available))
  const langfuseSecretSet = computed(() => Boolean(server.value?.langfuse_secret_key_set))

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
      msgPlaneMode.value = s.msg_plane_mode || 'pg'
      msgPlaneNATSURL.value = s.msg_plane_nats_url ?? ''
      msgPlaneSubject.value = s.msg_plane_subject || 'runs.dispatch'
      langfuseEnabled.value = Boolean(s.langfuse_enabled)
      langfuseBaseURL.value = s.langfuse_base_url ?? ''
      langfusePublicKey.value = s.langfuse_public_key ?? ''
      langfuseSecret.value = ''
    } catch (e) {
      serverError.value = toErrorMessage(e)
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
    // Edition-scoped keys ride only when their surface exists: the server
    // rejects them by PRESENCE on the other editions (the message plane on
    // personal forms, the Langfuse plugin on console-managed forms), which
    // would fail every save over fields the form never showed.
    if (msgPlaneAvailable.value) {
      patch.msg_plane_mode = msgPlaneMode.value
      patch.msg_plane_nats_url = msgPlaneNATSURL.value
      patch.msg_plane_subject = msgPlaneSubject.value
    }
    if (!langfuseConsoleManaged.value) {
      patch.langfuse_enabled = langfuseEnabled.value
      patch.langfuse_base_url = langfuseBaseURL.value
      patch.langfuse_public_key = langfusePublicKey.value
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
    if (langfuseSecret.value && !langfuseConsoleManaged.value) patch.langfuse_secret_key = langfuseSecret.value
    try {
      const res = await updateSettings(patch)
      restartRequired.value = res.restart_required
      savedMessage.value = res.restart_required
        ? i18n.global.t('settings.store.savedRestart')
        : res.apply_error
          ? i18n.global.t('settings.store.savedApplyError', { error: res.apply_error })
          : i18n.global.t('settings.store.saved')
      // The one pref↔server bridge: a saved server default model becomes
      // the preferred model.
      if (serverModel.value) useSettingsStore().model = serverModel.value
      await loadServer()
      // Provider/model changes may flip the thinking drawer's support state.
      void useAgentStore().load()
    } catch (e) {
      serverError.value = toErrorMessage(e)
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
      serverError.value = toErrorMessage(e)
    }
  }

  return {
    managedFields,
    isManaged,
    modelWhitelist,
    models,
    modelsLoaded,
    applyingModel,
    noModelsManaged,
    ensureModels,
    applyModel,
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
    checkpointBackend,
    memoryBackend,
    postgresURL,
    debugLog,
    msgPlaneMode,
    msgPlaneNATSURL,
    msgPlaneSubject,
    langfuseEnabled,
    langfuseBaseURL,
    langfusePublicKey,
    langfuseSecret,
    langfuseConsoleManaged,
    langfuseSecretSet,
    msgPlaneAvailable,
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
  }
})
