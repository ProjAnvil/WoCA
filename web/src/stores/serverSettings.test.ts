import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises } from '@vue/test-utils'
import i18n from '../i18n'

const api = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  // saveSection('provider') fire-and-forgets an agent reload
  // (useAgentStore().load → getAgent); mocking it here keeps that
  // cross-store load off the network instead of printing ECONNREFUSED
  // noise once the test's microtasks drain.
  getAgent: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import { SECTION_KEYS, useServerSettingsStore } from './serverSettings'

function settingsDoc(over: Record<string, unknown> = {}) {
  return { provider: 'anthropic', msg_plane_available: false, ...over }
}

// ---------------------------------------------------------------------------
// SECTION_KEYS coverage — the four explicit key lists the store's
// persistence path registers (the payload groups the old full-patch save
// assembled). Every key must land in a section: a miss would strand a field
// behind no save affordance at all.
// ---------------------------------------------------------------------------

// The always-on patch body (the store persistence path's base object).
const BASE_PATCH_KEYS = [
  'provider',
  'model',
  'anthropic_base_url',
  'openai_base_url',
  'glm_base_url',
  'deepseek_base_url',
  'checkpoint_backend',
  'memory_backend',
  'memory_engine',
  'ladym_embedding',
  'ladym_embedding_model',
  'ladym_embedding_base_url',
  'ladym_llm',
  'ladym_autoretain',
  'ladym_topk',
  'ladym_system2_interval',
  'ladym_db_path',
  'ladym_system2',
] as const

// Keys that ride only when the message-plane surface exists.
const MSG_PLANE_KEYS = ['msg_plane_mode', 'msg_plane_nats_url', 'msg_plane_subject'] as const

// Keys that ride only on personal (non console-managed) forms.
const LANGFUSE_KEYS = [
  'langfuse_enabled',
  'langfuse_base_url',
  'langfuse_public_key',
  'langfuse_secret_key',
] as const

// Secret keys: forwarded only when the user typed a non-empty draft.
const SECRET_KEYS = [
  'anthropic_api_key',
  'openai_api_key',
  'glm_api_key',
  'deepseek_api_key',
  'postgres_url',
  'ladym_embedding_api_key',
  'langfuse_secret_key',
] as const

const REGISTERED = [
  ...BASE_PATCH_KEYS,
  ...MSG_PLANE_KEYS,
  ...LANGFUSE_KEYS,
  ...SECRET_KEYS,
]

function sectionsOwning(key: string): string[] {
  return Object.entries(SECTION_KEYS)
    .filter(([, keys]) => keys.includes(key))
    .map(([id]) => id)
}

describe('serverSettings SECTION_KEYS registry', () => {
  it.each([...BASE_PATCH_KEYS])('base-patch key %s lands in a section', (key) => {
    expect(sectionsOwning(key).length).toBeGreaterThan(0)
  })

  it.each([...MSG_PLANE_KEYS])('message-plane key %s lands in a section', (key) => {
    expect(sectionsOwning(key).length).toBeGreaterThan(0)
  })

  it.each([...LANGFUSE_KEYS])('langfuse key %s lands in a section', (key) => {
    expect(sectionsOwning(key).length).toBeGreaterThan(0)
  })

  it.each([...SECRET_KEYS])('secret key %s lands in a section', (key) => {
    expect(sectionsOwning(key).length).toBeGreaterThan(0)
  })

  it('introduces no keys outside the registered lists', () => {
    const registered = REGISTERED as readonly string[]
    const extra = Object.values(SECTION_KEYS)
      .flat()
      .filter((k) => !registered.includes(k))
    expect(extra).toEqual([])
  })

  it('assigns every key to exactly one section (no double ownership)', () => {
    for (const key of REGISTERED) {
      expect(sectionsOwning(key)).toHaveLength(1)
    }
  })

  it('appearance and preferences are instant sections with no keys', () => {
    expect(SECTION_KEYS.appearance).toEqual([])
    expect(SECTION_KEYS.preferences).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// saveSection: same-endpoint subset PUT with the availability gating intact.
// ---------------------------------------------------------------------------

describe('serverSettings saveSection subset PUT', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
    api.getSettings.mockResolvedValue(settingsDoc())
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    api.getAgent.mockResolvedValue(undefined)
  })

  it('storage save PUTs only storage keys', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.checkpointBackend = 'postgres'
    s.postgresURL = 'postgres://u:p@h/db'
    await s.saveSection('storage')
    await flushPromises()
    expect(api.updateSettings).toHaveBeenCalledTimes(1)
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(Object.keys(patch).sort()).toEqual(
      ['checkpoint_backend', 'memory_backend', 'postgres_url'].sort(),
    )
    expect(patch.checkpoint_backend).toBe('postgres')
    expect(patch.postgres_url).toBe('postgres://u:p@h/db')
  })

  it('provider save forwards only the typed secret, not the empty drafts', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.apiKey = 'sk-ant-new'
    await s.saveSection('provider')
    await flushPromises()
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(patch.anthropic_api_key).toBe('sk-ant-new')
    expect(patch.openai_api_key).toBeUndefined()
    expect(patch.glm_api_key).toBeUndefined()
    expect(patch.deepseek_api_key).toBeUndefined()
    expect(patch.postgres_url).toBeUndefined()
  })

  it('sends exactly the msg-plane keys on a plane-available form', async () => {
    const s = useServerSettingsStore()
    api.getSettings.mockResolvedValue(settingsDoc({ msg_plane_available: true }))
    await s.loadServer()
    s.msgPlaneSubject = 'runs.wake'
    await s.saveSection('msgplane')
    await flushPromises()
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(Object.keys(patch).sort()).toEqual(
      ['msg_plane_mode', 'msg_plane_nats_url', 'msg_plane_subject'].sort(),
    )
    expect(patch.msg_plane_subject).toBe('runs.wake')
  })

  it('keeps msg-plane keys out of every section subset when the plane is off', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.serverModel = 'glm-5'
    s.checkpointBackend = 'postgres'
    await s.saveSection('provider')
    await s.saveSection('storage')
    await flushPromises()
    for (const call of api.updateSettings.mock.calls) {
      const patch = call[0] as Record<string, unknown>
      for (const key of Object.keys(patch)) {
        expect(key.startsWith('msg_plane_')).toBe(false)
      }
    }
  })

  it('skips the request entirely when gating empties the subset', async () => {
    const s = useServerSettingsStore()
    // Plane off + console-managed langfuse: neither EE surface accepts keys.
    api.getSettings.mockResolvedValue(
      settingsDoc({ msg_plane_available: false, langfuse_console_managed: true }),
    )
    await s.loadServer()
    await s.saveSection('msgplane')
    await s.saveSection('langfuse')
    await flushPromises()
    expect(api.updateSettings).not.toHaveBeenCalled()
  })

  it('sends langfuse keys on the personal form', async () => {
    const s = useServerSettingsStore()
    api.getSettings.mockResolvedValue(settingsDoc({ langfuse_console_managed: false }))
    await s.loadServer()
    s.langfuseEnabled = true
    s.langfuseBaseURL = 'https://lf.example'
    await s.saveSection('langfuse')
    await flushPromises()
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(patch.langfuse_enabled).toBe(true)
    expect(patch.langfuse_base_url).toBe('https://lf.example')
    expect(Object.keys(patch).every((k) => k.startsWith('langfuse_'))).toBe(true)
  })
})

// ---------------------------------------------------------------------------
// sectionDirty lifecycle.
// ---------------------------------------------------------------------------

describe('serverSettings sectionDirty lifecycle', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
    api.getSettings.mockResolvedValue(settingsDoc())
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    api.getAgent.mockResolvedValue(undefined)
  })

  it('is false after load, true after an edit, false again after the section save', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    expect(s.sectionDirty('storage')).toBe(false)
    expect(s.sectionDirty('provider')).toBe(false)
    expect(s.sectionDirty('memory')).toBe(false)

    s.checkpointBackend = 'postgres'
    expect(s.sectionDirty('storage')).toBe(true)
    // Another section's edit stays out of this section's dirty state.
    expect(s.sectionDirty('provider')).toBe(false)

    s.serverModel = 'glm-5'
    expect(s.sectionDirty('provider')).toBe(true)

    await s.saveSection('storage')
    await flushPromises()
    expect(s.sectionDirty('storage')).toBe(false)
    // The provider draft was never saved — still dirty.
    expect(s.sectionDirty('provider')).toBe(true)
  })

  it('tracks secret drafts: typing marks dirty, the post-save reload clears', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    expect(s.sectionDirty('provider')).toBe(false)
    s.apiKey = 'sk-ant-2'
    expect(s.sectionDirty('provider')).toBe(true)
    await s.saveSection('provider')
    await flushPromises()
    expect(s.sectionDirty('provider')).toBe(false)
  })

  it('instant sections are never dirty', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    expect(s.sectionDirty('appearance')).toBe(false)
    expect(s.sectionDirty('preferences')).toBe(false)
  })

  it('stays dirty when the save request fails, and clears on a successful retry', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.checkpointBackend = 'postgres'
    expect(s.sectionDirty('storage')).toBe(true)
    api.updateSettings.mockRejectedValueOnce(new Error('network down'))
    await s.saveSection('storage')
    await flushPromises()
    // A failed PUT persists nothing: the draft and its dirty flag survive.
    expect(s.serverError).toBeTruthy()
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.sectionDirty('storage')).toBe(true)
    await s.saveSection('storage')
    await flushPromises()
    expect(s.sectionDirty('storage')).toBe(false)
  })

  it('keeps edits made to the saving section while its save is in flight, and flags it dirty', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.checkpointBackend = 'postgres'
    let resolvePut!: (v: unknown) => void
    api.updateSettings.mockReturnValueOnce(new Promise((r) => (resolvePut = r)))
    const saving = s.saveSection('storage')
    // The user keeps typing while the PUT is in flight — the payload was
    // already built, so this edit never rode the request.
    s.postgresURL = 'postgres://midflight'
    // The server echoes the saved subset back on the reload.
    api.getSettings.mockResolvedValue(settingsDoc({ checkpoint_backend: 'postgres' }))
    resolvePut({ ok: true, restart_required: false })
    await saving
    await flushPromises()
    expect(s.postgresURL).toBe('postgres://midflight')
    expect(s.sectionDirty('storage')).toBe(true)
  })

  it('ends clean when the saving section is left alone mid-flight', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.checkpointBackend = 'postgres'
    let resolvePut!: (v: unknown) => void
    api.updateSettings.mockReturnValueOnce(new Promise((r) => (resolvePut = r)))
    const saving = s.saveSection('storage')
    api.getSettings.mockResolvedValue(settingsDoc({ checkpoint_backend: 'postgres' }))
    resolvePut({ ok: true, restart_required: false })
    await saving
    await flushPromises()
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.sectionDirty('storage')).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// toggleDebugLog instant apply: draft preservation across the reload it
// triggers (capture-all semantics — see captureUnsavedDrafts).
// ---------------------------------------------------------------------------

describe('serverSettings toggleDebugLog draft preservation', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
    api.getSettings.mockResolvedValue(settingsDoc())
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    api.getAgent.mockResolvedValue(undefined)
  })

  it('preserves the drafts of every dirty section across the reload it triggers', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.serverModel = 'glm-5' // provider draft
    s.checkpointBackend = 'postgres' // storage draft
    const toggling = s.toggleDebugLog()
    // The reload after the instant apply echoes the flipped flag only —
    // the two dirty sections' drafts must survive it.
    api.getSettings.mockResolvedValue(settingsDoc({ debug_log: true }))
    await toggling
    await flushPromises()
    expect(s.debugLog).toBe(true)
    expect(s.serverModel).toBe('glm-5')
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.sectionDirty('provider')).toBe(true)
    expect(s.sectionDirty('storage')).toBe(true)
  })
})

// ---------------------------------------------------------------------------
// loadServer: edits made while a reload fetch is in flight (and unsaved
// drafts sitting in the form when a reload lands, e.g. a settings-panel
// reopen) are never silently reverted.
// ---------------------------------------------------------------------------

describe('serverSettings loadServer in-flight edit preservation', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
    api.getSettings.mockResolvedValue(settingsDoc())
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    api.getAgent.mockResolvedValue(undefined)
  })

  it('keeps edits typed while the fetch is in flight, and flags the section dirty', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    let resolveGet!: (v: unknown) => void
    api.getSettings.mockReturnValueOnce(new Promise((r) => (resolveGet = r)))
    const loading = s.loadServer()
    // Typed while the fetch is in flight — the response would clobber it.
    s.checkpointBackend = 'postgres'
    resolveGet(settingsDoc({ provider: 'glm' }))
    await loading
    await flushPromises()
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.sectionDirty('storage')).toBe(true)
    // Untouched sections still take the server's truth verbatim.
    expect(s.provider).toBe('glm')
    expect(s.sectionDirty('provider')).toBe(false)
  })

  it('keeps edits typed during the FIRST load, before any snapshot exists', async () => {
    const s = useServerSettingsStore()
    let resolveGet!: (v: unknown) => void
    api.getSettings.mockReturnValueOnce(new Promise((r) => (resolveGet = r)))
    const loading = s.loadServer()
    s.serverModel = 'glm-5'
    resolveGet(settingsDoc({ model: 'claude-sonnet-4-5' }))
    await loading
    await flushPromises()
    expect(s.serverModel).toBe('glm-5')
    expect(s.sectionDirty('provider')).toBe(true)
  })

  it('keeps a section across a reload even when its edit predates the fetch', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    s.serverModel = 'glm-5'
    // e.g. the settings panel reopening with unsaved work in the form.
    await s.loadServer()
    await flushPromises()
    expect(s.serverModel).toBe('glm-5')
    expect(s.sectionDirty('provider')).toBe(true)
  })

  it('reflects server truth for untouched sections and ends clean', async () => {
    const s = useServerSettingsStore()
    await s.loadServer()
    api.getSettings.mockResolvedValue(settingsDoc({ checkpoint_backend: 'postgres', provider: 'glm' }))
    await s.loadServer()
    await flushPromises()
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.provider).toBe('glm')
    expect(s.sectionDirty('storage')).toBe(false)
    expect(s.sectionDirty('provider')).toBe(false)
  })

  it('genuine first paint (no snapshot, no edits) ends clean with server values', async () => {
    const s = useServerSettingsStore()
    api.getSettings.mockResolvedValue(
      settingsDoc({
        provider: 'glm',
        model: 'glm-5',
        checkpoint_backend: 'postgres',
        memory_engine: 'remote',
        msg_plane_nats_url: 'nats://h:4222',
        langfuse_base_url: 'https://lf.example',
      }),
    )
    await s.loadServer()
    await flushPromises()
    // No drift past the dispatch baseline → every section takes the
    // server's values verbatim and nothing is dirty.
    for (const id of Object.keys(SECTION_KEYS) as (keyof typeof SECTION_KEYS)[]) {
      expect(s.sectionDirty(id)).toBe(false)
    }
    expect(s.provider).toBe('glm')
    expect(s.serverModel).toBe('glm-5')
    expect(s.checkpointBackend).toBe('postgres')
    expect(s.memoryEngine).toBe('remote')
    expect(s.msgPlaneNATSURL).toBe('nats://h:4222')
    expect(s.langfuseBaseURL).toBe('https://lf.example')
  })
})
