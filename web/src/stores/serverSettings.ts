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
import { useToastStore } from './toast'

// --- per-section persistence (F2 · variant A) ---
// Which settings keys each panel section owns. Empty arrays are the
// instant-apply sections (client-side only, nothing to persist). The four
// explicit key groups the old full-patch save assembled are fully covered:
// the always-on base patch, the msg-plane keys (EE surface), the langfuse
// keys (personal form) and the typed-secret keys.
export type SectionId =
  | 'appearance'
  | 'preferences'
  | 'provider'
  | 'storage'
  | 'memory'
  | 'msgplane'
  | 'langfuse'

export const SECTION_KEYS: Record<SectionId, readonly string[]> = {
  appearance: [],
  preferences: [],
  provider: [
    'provider',
    'model',
    'anthropic_base_url',
    'openai_base_url',
    'glm_base_url',
    'deepseek_base_url',
    'anthropic_api_key',
    'openai_api_key',
    'glm_api_key',
    'deepseek_api_key',
  ],
  storage: ['checkpoint_backend', 'memory_backend', 'postgres_url'],
  memory: [
    'memory_engine',
    'ladym_embedding',
    'ladym_embedding_model',
    'ladym_embedding_base_url',
    'ladym_embedding_api_key',
    'ladym_llm',
    'ladym_autoretain',
    'ladym_topk',
    'ladym_db_path',
    'ladym_system2',
    'ladym_system2_interval',
  ],
  msgplane: ['msg_plane_mode', 'msg_plane_nats_url', 'msg_plane_subject'],
  langfuse: [
    'langfuse_enabled',
    'langfuse_base_url',
    'langfuse_public_key',
    'langfuse_secret_key',
  ],
}

// Secret keys ride the PUT only when the user typed a non-empty draft (an
// empty password input means "keep the stored key", never "clear it").
const SECRET_KEYS: ReadonlySet<string> = new Set([
  'anthropic_api_key',
  'openai_api_key',
  'glm_api_key',
  'deepseek_api_key',
  'postgres_url',
  'ladym_embedding_api_key',
  'langfuse_secret_key',
])

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
  // Which section's save is in flight (double-submit guard): only that
  // section's button disables — never a pre-disable validation gate.
  const savingSection = ref<SectionId | null>(null)

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
  // set — see apiKeySet), so saveSection only forwards non-empty secrets
  // and loadServer resets the drafts to ''. Turning an empty string into
  // an explicit clear would let a casual "save" destroy an encrypted key
  // the form never even displayed.
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
      snapshotSections()
    } catch (e) {
      serverError.value = toErrorMessage(e)
    }
  }

  // --- per-section dirty tracking (spec OQ3) ---
  // Key↔draft wiring as one table: the dirty snapshot, the subset PUT and
  // the cross-section draft preservation all read and write through it.
  type Prim = string | number | boolean
  const draftAccessors: Record<string, { get: () => Prim; set: (v: Prim) => void }> = {
    provider: { get: () => provider.value, set: (v) => (provider.value = v as ProviderName) },
    model: { get: () => serverModel.value, set: (v) => (serverModel.value = String(v)) },
    anthropic_base_url: {
      get: () => baseURLs.value.anthropic,
      set: (v) => (baseURLs.value.anthropic = String(v)),
    },
    openai_base_url: {
      get: () => baseURLs.value.openai,
      set: (v) => (baseURLs.value.openai = String(v)),
    },
    glm_base_url: {
      get: () => baseURLs.value.glm,
      set: (v) => (baseURLs.value.glm = String(v)),
    },
    deepseek_base_url: {
      get: () => baseURLs.value.deepseek,
      set: (v) => (baseURLs.value.deepseek = String(v)),
    },
    anthropic_api_key: {
      get: () => apiKeys.value.anthropic,
      set: (v) => (apiKeys.value.anthropic = String(v)),
    },
    openai_api_key: {
      get: () => apiKeys.value.openai,
      set: (v) => (apiKeys.value.openai = String(v)),
    },
    glm_api_key: {
      get: () => apiKeys.value.glm,
      set: (v) => (apiKeys.value.glm = String(v)),
    },
    deepseek_api_key: {
      get: () => apiKeys.value.deepseek,
      set: (v) => (apiKeys.value.deepseek = String(v)),
    },
    checkpoint_backend: {
      get: () => checkpointBackend.value,
      set: (v) => (checkpointBackend.value = v as CheckpointBackend),
    },
    memory_backend: {
      get: () => memoryBackend.value,
      set: (v) => (memoryBackend.value = v as MemoryBackend),
    },
    postgres_url: { get: () => postgresURL.value, set: (v) => (postgresURL.value = String(v)) },
    memory_engine: {
      get: () => memoryEngine.value,
      set: (v) => (memoryEngine.value = v as 'ladym' | 'remote' | 'none'),
    },
    ladym_embedding: {
      get: () => ladymEmbedding.value,
      set: (v) => (ladymEmbedding.value = String(v)),
    },
    ladym_embedding_model: {
      get: () => ladymEmbeddingModel.value,
      set: (v) => (ladymEmbeddingModel.value = String(v)),
    },
    ladym_embedding_base_url: {
      get: () => ladymEmbeddingBaseURL.value,
      set: (v) => (ladymEmbeddingBaseURL.value = String(v)),
    },
    ladym_embedding_api_key: {
      get: () => ladymEmbeddingAPIKey.value,
      set: (v) => (ladymEmbeddingAPIKey.value = String(v)),
    },
    ladym_llm: { get: () => ladymLLM.value, set: (v) => (ladymLLM.value = String(v)) },
    ladym_autoretain: {
      get: () => ladymAutoRetain.value,
      set: (v) => (ladymAutoRetain.value = Boolean(v)),
    },
    ladym_topk: { get: () => ladymTopK.value, set: (v) => (ladymTopK.value = Number(v)) },
    ladym_db_path: { get: () => ladymDBPath.value, set: (v) => (ladymDBPath.value = String(v)) },
    ladym_system2: {
      get: () => ladymSystem2.value,
      set: (v) => (ladymSystem2.value = Boolean(v)),
    },
    ladym_system2_interval: {
      get: () => ladymSystem2Interval.value,
      set: (v) => (ladymSystem2Interval.value = Number(v)),
    },
    msg_plane_mode: {
      get: () => msgPlaneMode.value,
      set: (v) => (msgPlaneMode.value = v as 'pg' | 'pg+nats'),
    },
    msg_plane_nats_url: {
      get: () => msgPlaneNATSURL.value,
      set: (v) => (msgPlaneNATSURL.value = String(v)),
    },
    msg_plane_subject: {
      get: () => msgPlaneSubject.value,
      set: (v) => (msgPlaneSubject.value = String(v)),
    },
    langfuse_enabled: {
      get: () => langfuseEnabled.value,
      set: (v) => (langfuseEnabled.value = Boolean(v)),
    },
    langfuse_base_url: {
      get: () => langfuseBaseURL.value,
      set: (v) => (langfuseBaseURL.value = String(v)),
    },
    langfuse_public_key: {
      get: () => langfusePublicKey.value,
      set: (v) => (langfusePublicKey.value = String(v)),
    },
    langfuse_secret_key: {
      get: () => langfuseSecret.value,
      set: (v) => (langfuseSecret.value = String(v)),
    },
  }

  function draftValue(key: string): Prim {
    return draftAccessors[key]?.get() ?? ''
  }

  // loadServer records each section's grouped draft values; sectionDirty
  // compares the live drafts against the snapshot. Before the first load
  // there is nothing to be dirty against.
  const sectionSnapshots = ref<
    Partial<Record<SectionId, Record<string, string | number | boolean>>>
  >({})

  function snapshotSections() {
    const snap: Partial<Record<SectionId, Record<string, string | number | boolean>>> = {}
    for (const id of Object.keys(SECTION_KEYS) as SectionId[]) {
      const keys = SECTION_KEYS[id]
      if (!keys.length) continue // instant sections carry no drafts
      const values: Record<string, string | number | boolean> = {}
      for (const key of keys) values[key] = draftValue(key)
      snap[id] = values
    }
    sectionSnapshots.value = snap
  }

  function sectionDirty(id: SectionId): boolean {
    const snap = sectionSnapshots.value[id]
    if (!snap) return false
    return SECTION_KEYS[id].some((key) => !Object.is(draftValue(key), snap[key]))
  }

  // Drafts of every dirty section except `except` — the section whose save
  // is about to trigger a reload (its own drafts are handled separately,
  // see saveSection's mid-flight pass). Omitting `except` captures every
  // dirty section: the instant-apply paths own no section drafts of their
  // own, so they must preserve everything.
  function captureUnsavedDrafts(except?: SectionId): Record<string, Prim> {
    const out: Record<string, Prim> = {}
    for (const id of Object.keys(SECTION_KEYS) as SectionId[]) {
      if (id === except || !sectionDirty(id)) continue
      for (const key of SECTION_KEYS[id]) out[key] = draftValue(key)
    }
    return out
  }

  // An emptied numeric input yields ''/NaN, which the server would reject
  // with a 400 — fall back to the documented defaults.
  function patchValue(key: string, v: string | number | boolean): string | number | boolean {
    if (key === 'ladym_topk') {
      return typeof v === 'number' && Number.isFinite(v) && v >= 1 ? v : 6
    }
    if (key === 'ladym_system2_interval') {
      return typeof v === 'number' && Number.isFinite(v) && v >= 60 ? v : 300
    }
    return v
  }

  // Per-section save: same endpoint, subset PUT. The server treats absent
  // keys as unchanged, so only this section's keys ride the patch.
  async function saveSection(id: SectionId): Promise<void> {
    if (savingSection.value) return
    const keys = SECTION_KEYS[id]
    if (!keys.length) return // instant sections never hit the endpoint
    const patch: Record<string, string | number | boolean> = {}
    for (const key of keys) {
      // Edition-scoped keys ride only when their surface exists: the server
      // rejects them by PRESENCE on the other editions (the message plane
      // on personal forms, the Langfuse plugin on console-managed forms).
      if (key.startsWith('msg_plane_') && !msgPlaneAvailable.value) continue
      if (key.startsWith('langfuse_') && langfuseConsoleManaged.value) continue
      const v = draftValue(key)
      // Secrets: only send what the user actually typed. An empty draft
      // means "keep the stored key" — the server treats absent fields as
      // unchanged, and forwarding '' would wipe an encrypted key the form
      // never showed.
      if (SECRET_KEYS.has(key) && !v) continue
      patch[key] = patchValue(key, v)
    }
    // A fully gated-out subset (EE surface absent) has nothing to persist.
    if (!Object.keys(patch).length) return
    savingSection.value = id
    serverError.value = ''
    // Baseline of the saving section's drafts at dispatch time. Any draft
    // that has moved past this baseline when the request settles is a
    // mid-flight edit the payload never carried (the patch object was
    // already built).
    const sentDrafts: Record<string, Prim> = {}
    for (const key of keys) sentDrafts[key] = draftValue(key)
    const toast = useToastStore()
    try {
      const res = await updateSettings(patch as AppSettingsUpdate)
      // Results ride the app toast stack (role="status" + aria-live) —
      // the shared announcement channel, not a parallel mechanism.
      toast.success(
        res.restart_required
          ? i18n.global.t('settings.store.savedRestart')
          : res.apply_error
            ? i18n.global.t('settings.store.savedApplyError', { error: res.apply_error })
            : i18n.global.t('settings.store.saved'),
      )
      // The one pref↔server bridge: a saved server default model becomes
      // the preferred model (the provider section owns the `model` key).
      if (id === 'provider' && serverModel.value) {
        useSettingsStore().model = serverModel.value
      }
      // A subset save must not wipe unsaved drafts in OTHER sections: the
      // server still holds their pre-edit values, so capture each dirty
      // section's drafts and restore them after the reload (loadServer's
      // fresh snapshot then keeps those sections correctly dirty).
      const unsaved = captureUnsavedDrafts(id)
      // Edits made to THIS section while its own save was in flight never
      // rode the PUT, so the reload would silently revert them. Capture the
      // drift past the sent baseline and re-apply it after the reload;
      // against the fresh snapshot it correctly marks the section dirty
      // again. A section left alone mid-flight has no drift and settles
      // clean, exactly as before.
      const inFlightEdits: Record<string, Prim> = {}
      for (const key of keys) {
        const v = draftValue(key)
        if (!Object.is(v, sentDrafts[key])) inFlightEdits[key] = v
      }
      await loadServer()
      for (const [key, v] of Object.entries(unsaved)) draftAccessors[key]?.set(v)
      for (const [key, v] of Object.entries(inFlightEdits)) draftAccessors[key]?.set(v)
      // Provider/model changes may flip the thinking drawer's support state.
      if (id === 'provider') void useAgentStore().load()
    } catch (e) {
      serverError.value = toErrorMessage(e)
      toast.error(serverError.value)
    } finally {
      savingSection.value = null
    }
  }

  async function toggleDebugLog() {
    const next = !debugLog.value
    try {
      await updateSettings({ debug_log: next })
      debugLog.value = next
      // The instant apply must not wipe unsaved drafts in ANY section (same
      // preservation as a subset save). debug_log owns no section drafts, so
      // this captures everything dirty — explicitly, not by excluding a
      // section that merely happens to be empty today.
      const unsaved = captureUnsavedDrafts()
      await loadServer()
      for (const [key, v] of Object.entries(unsaved)) draftAccessors[key]?.set(v)
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
    savingSection,
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
    saveSection,
    sectionDirty,
  }
})
