import { defineStore } from 'pinia'
import { useDark, useToggle } from '@vueuse/core'
import { computed, ref } from 'vue'
import { getSettings, updateSettings } from '../api/client'
import type { AppSettings, AppSettingsUpdate } from '../types'

// Settings + appearance state. Theme is a useDark singleton (persists to
// localStorage under "woca-theme" and toggles the `.dark` class on <html>).
// Provider/storage settings are persisted server-side via /api/settings
// (sqlite under ~/.woca by default); API keys are stored encrypted.
export const useSettingsStore = defineStore('settings', () => {
  const isDark = useDark({ storageKey: 'woca-theme' })
  const toggleDark = useToggle(isDark)

  const panelOpen = ref(false)

  // Demo preferences (cards in the settings panel).
  const autoCollapseTools = ref(false)
  const markdownPreview = ref(true)
  const model = ref('claude-sonnet-4-5')
  const permissionMode = ref<'default' | 'acceptEdits' | 'bypassPermissions' | 'readOnly' | 'plan'>('default')

  // --- server-backed settings ---
  const server = ref<AppSettings | null>(null)
  const serverError = ref('')
  const saving = ref(false)
  const savedMessage = ref('')
  const restartRequired = ref(false)

  // Local editable copies for the form (password inputs stay empty unless
  // the user types a new value).
  const provider = ref<'anthropic' | 'openai'>('anthropic')
  const serverModel = ref('')
  const anthropicBaseURL = ref('')
  const openaiBaseURL = ref('')
  const anthropicAPIKey = ref('')
  const openaiAPIKey = ref('')
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

  // Unified credential fields: one Base URL + one API Key input that target
  // the selected provider's stored fields (the user picks a provider, not a
  // per-provider form). Drafts are kept per provider so switching back and
  // forth does not lose typed values.
  const baseURL = computed({
    get: () => (provider.value === 'anthropic' ? anthropicBaseURL.value : openaiBaseURL.value),
    set: (v: string) => {
      if (provider.value === 'anthropic') anthropicBaseURL.value = v
      else openaiBaseURL.value = v
    },
  })
  const apiKey = computed({
    get: () => (provider.value === 'anthropic' ? anthropicAPIKey.value : openaiAPIKey.value),
    set: (v: string) => {
      if (provider.value === 'anthropic') anthropicAPIKey.value = v
      else openaiAPIKey.value = v
    },
  })
  const apiKeySet = computed(() =>
    provider.value === 'anthropic'
      ? Boolean(server.value?.anthropic_api_key_set)
      : Boolean(server.value?.openai_api_key_set),
  )
  const ladymEmbeddingKeySet = computed(() => Boolean(server.value?.ladym_embedding_api_key_set))

  async function loadServer() {
    serverError.value = ''
    try {
      const s = await getSettings()
      server.value = s
      provider.value = s.provider
      serverModel.value = s.model
      anthropicBaseURL.value = s.anthropic_base_url
      openaiBaseURL.value = s.openai_base_url
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
      ladymEmbeddingAPIKey.value = ''
      anthropicAPIKey.value = ''
      openaiAPIKey.value = ''
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
      anthropic_base_url: anthropicBaseURL.value,
      openai_base_url: openaiBaseURL.value,
      checkpoint_backend: checkpointBackend.value,
      memory_backend: memoryBackend.value,
      memory_engine: memoryEngine.value,
      ladym_embedding: ladymEmbedding.value,
      ladym_embedding_model: ladymEmbeddingModel.value,
      ladym_embedding_base_url: ladymEmbeddingBaseURL.value,
      ladym_llm: ladymLLM.value,
      ladym_autoretain: ladymAutoRetain.value,
      ladym_topk: ladymTopK.value,
      ladym_db_path: ladymDBPath.value,
      ladym_system2: ladymSystem2.value,
    }
    // Secrets: only send when the user typed something (empty = unchanged).
    if (anthropicAPIKey.value) patch.anthropic_api_key = anthropicAPIKey.value
    if (openaiAPIKey.value) patch.openai_api_key = openaiAPIKey.value
    if (postgresURL.value) patch.postgres_url = postgresURL.value
    if (ladymEmbeddingAPIKey.value) patch.ladym_embedding_api_key = ladymEmbeddingAPIKey.value
    try {
      const res = await updateSettings(patch)
      restartRequired.value = res.restart_required
      savedMessage.value = res.restart_required
        ? '已保存；存储后端变更需重启 woca 生效'
        : res.apply_error
          ? `已保存，但模型切换失败：${res.apply_error}`
          : '已保存'
      if (serverModel.value) model.value = serverModel.value
      await loadServer()
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
    logDir: computed(() => server.value?.log_dir ?? ''),
    toggleDebugLog,
    loadServer,
    saveServer,
    openPanel,
    closePanel,
  }
})
