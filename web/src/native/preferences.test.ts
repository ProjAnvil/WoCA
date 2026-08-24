import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const storedNativePreferences: Record<string, string> = {
  'woca-theme': 'dark',
  'woca.pref.locale': 'en',
  'woca.pref.autoCollapseTools': 'false',
  'woca.pref.markdownPreview': 'true',
  'woca.pref.model': 'native-model',
  'woca.pref.permissionMode': 'readOnly',
}

function bridgeWithPreferences(overrides: Partial<NativeAppBridge> = {}): NativeAppBridge {
  return {
    Bootstrap: vi.fn<NativeAppBridge['Bootstrap']>().mockResolvedValue({ state: 'ready' }),
    Connect: vi.fn<NativeAppBridge['Connect']>().mockResolvedValue({ state: 'ready' }),
    Logout: vi.fn<NativeAppBridge['Logout']>().mockResolvedValue({ state: 'authorization_required' }),
    ConnectEmbedded: vi.fn<NativeAppBridge['ConnectEmbedded']>().mockResolvedValue({ state: 'ready' }),
    EdgeBridges: vi.fn<NativeAppBridge['EdgeBridges']>().mockResolvedValue([]),
    StreamCredentials: vi.fn<NativeAppBridge['StreamCredentials']>().mockResolvedValue({ scheme: '', token: '' }),
    Preference: vi.fn(async (key: string) => ({
      value: storedNativePreferences[key] ?? '',
      found: key in storedNativePreferences,
    })),
    SetPreference: vi.fn(async (input) => ({ revision: input.revision, applied: true })),
    CancelRequest: vi.fn(async () => true),
    ...overrides,
  }
}

describe('native-aware preferences', () => {
  beforeEach(() => {
    vi.resetModules()
    window.localStorage.clear()
    window.sessionStorage.clear()
    vi.stubGlobal('go', undefined)
    document.documentElement.classList.remove('dark')
  })

  it('preserves the browser localStorage keys and synchronous values', async () => {
    window.localStorage.setItem('woca-theme', 'dark')
    window.localStorage.setItem('woca.pref.locale', 'en')
    window.localStorage.setItem('woca.pref.autoCollapseTools', 'false')
    window.localStorage.setItem('woca.pref.markdownPreview', 'true')
    window.localStorage.setItem('woca.pref.model', 'browser-model')
    window.localStorage.setItem('woca.pref.permissionMode', 'plan')

    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    expect(settings.isDark).toBe(true)
    await nextTick()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(settings.localePref).toBe('en')
    expect(settings.autoCollapseTools).toBe(false)
    expect(settings.markdownPreview).toBe(true)
    expect(settings.model).toBe('browser-model')
    expect(settings.permissionMode).toBe('plan')

    settings.isDark = false
    settings.setLocale('zh-CN')
    settings.autoCollapseTools = true
    settings.markdownPreview = false
    settings.model = 'updated-browser-model'
    settings.permissionMode = 'acceptEdits'
    await nextTick()

    expect(window.localStorage.getItem('woca-theme')).toBe('auto')
    expect(window.localStorage.getItem('woca.pref.locale')).toBe('zh-CN')
    expect(window.localStorage.getItem('woca.pref.autoCollapseTools')).toBe('true')
    expect(window.localStorage.getItem('woca.pref.markdownPreview')).toBe('false')
    expect(window.localStorage.getItem('woca.pref.model')).toBe('updated-browser-model')
    expect(window.localStorage.getItem('woca.pref.permissionMode')).toBe('acceptEdits')
  })

  it('hydrates native typed preferences and persists writes without Web Storage', async () => {
    const bridge = bridgeWithPreferences()
    vi.stubGlobal('go', { main: { App: bridge } })
    const localGet = vi.spyOn(window.localStorage, 'getItem')
    const localSet = vi.spyOn(window.localStorage, 'setItem')
    const sessionGet = vi.spyOn(window.sessionStorage, 'getItem')
    const sessionSet = vi.spyOn(window.sessionStorage, 'setItem')

    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)
    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    expect(settings.isDark).toBe(true)
    await nextTick()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(settings.localePref).toBe('en')
    expect(settings.autoCollapseTools).toBe(false)
    expect(settings.markdownPreview).toBe(true)
    expect(settings.model).toBe('native-model')
    expect(settings.permissionMode).toBe('readOnly')

    settings.isDark = false
    settings.setLocale('zh-CN')
    settings.autoCollapseTools = true
    settings.markdownPreview = false
    settings.model = 'updated-native-model'
    settings.permissionMode = 'bypassPermissions'
    await nextTick()
    await preferences.drainPreferenceWrites()

    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'woca-theme', value: 'auto', revision: expect.any(Number) }),
    )
    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'woca.pref.locale', value: 'zh-CN', revision: expect.any(Number) }),
    )
    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'woca.pref.autoCollapseTools', value: 'true', revision: expect.any(Number) }),
    )
    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'woca.pref.markdownPreview', value: 'false', revision: expect.any(Number) }),
    )
    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'woca.pref.model', value: 'updated-native-model', revision: expect.any(Number) }),
    )
    expect(bridge.SetPreference).toHaveBeenCalledWith(
      expect.objectContaining({
        key: 'woca.pref.permissionMode',
        value: 'bypassPermissions',
        revision: expect.any(Number),
      }),
    )
    expect(localGet).not.toHaveBeenCalled()
    expect(localSet).not.toHaveBeenCalled()
    expect(sessionGet).not.toHaveBeenCalled()
    expect(sessionSet).not.toHaveBeenCalled()
  })

  it('uses typed defaults when native preferences are missing', async () => {
    const bridge = bridgeWithPreferences({
      Preference: vi.fn(async () => ({ value: '', found: false })),
    })
    vi.stubGlobal('go', { main: { App: bridge } })

    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)
    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    expect(typeof settings.isDark).toBe('boolean')
    expect(settings.localePref).toBe('auto')
    expect(settings.autoCollapseTools).toBe(true)
    expect(settings.markdownPreview).toBe(true)
    expect(settings.model).toBe('claude-sonnet-4-5')
    expect(settings.permissionMode).toBe('default')
  })

  it('rolls back a failed latest write and retries the retained desired value', async () => {
    const setPreference = vi
      .fn<NativeAppBridge['SetPreference']>()
      .mockRejectedValueOnce(new Error('secret value: updated-native-model'))
      .mockImplementation(async (input) => ({ revision: input.revision, applied: true }))
    const bridge = bridgeWithPreferences({ SetPreference: setPreference })
    vi.stubGlobal('go', { main: { App: bridge } })
    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)

    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    settings.model = 'updated-native-model'
    await nextTick()
    expect(settings.model).toBe('updated-native-model')
    expect(preferences.readPreference('woca.pref.model')).toBe('updated-native-model')

    await expect(preferences.drainPreferenceWrites()).rejects.toThrow('unable to persist native preferences')
    await nextTick()
    expect(settings.model).toBe('native-model')
    expect(preferences.readPreference('woca.pref.model')).toBe('native-model')

    expect(preferences.retryFailedNativePreference('woca.pref.model')).toBe(true)
    await nextTick()
    expect(settings.model).toBe('updated-native-model')
    await preferences.drainPreferenceWrites()
    expect(preferences.readPreference('woca.pref.model')).toBe('updated-native-model')
  })

  it('treats a stale SQLite CAS result as retryable without claiming persistence', async () => {
    const setPreference = vi
      .fn<NativeAppBridge['SetPreference']>()
      .mockImplementationOnce(async (input) => ({ revision: input.revision, applied: false }))
      .mockImplementation(async (input) => ({ revision: input.revision, applied: true }))
    const bridge = bridgeWithPreferences({ SetPreference: setPreference })
    vi.stubGlobal('go', { main: { App: bridge } })
    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)

    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    settings.model = 'cas-rejected-model'
    await nextTick()
    await expect(preferences.drainPreferenceWrites()).rejects.toThrow('unable to persist native preferences')
    await nextTick()
    expect(settings.model).toBe('native-model')
    expect(preferences.readPreference('woca.pref.model')).toBe('native-model')

    expect(preferences.retryFailedNativePreference('woca.pref.model')).toBe(true)
    await nextTick()
    expect(settings.model).toBe('cas-rejected-model')
    await preferences.drainPreferenceWrites()
    expect(preferences.readPreference('woca.pref.model')).toBe('cas-rejected-model')
    expect(setPreference).toHaveBeenCalledTimes(2)
  })

  it('restores a missing theme to its typed default after rejection and reapplies it on retry', async () => {
    let darkAttempts = 0
    const setPreference = vi.fn<NativeAppBridge['SetPreference']>().mockImplementation(async (input) => {
      if (input.key === 'woca-theme' && input.value === 'dark' && darkAttempts++ === 0) {
        throw new Error('secret persistence detail')
      }
      return { revision: input.revision, applied: true }
    })
    const bridge = bridgeWithPreferences({
      Preference: vi.fn(async (key: string) => ({
        value: key === 'woca-theme' ? '' : storedNativePreferences[key] ?? '',
        found: key !== 'woca-theme' && key in storedNativePreferences,
      })),
      SetPreference: setPreference,
    })
    vi.stubGlobal('go', { main: { App: bridge } })
    const localGet = vi.spyOn(window.localStorage, 'getItem')
    const localSet = vi.spyOn(window.localStorage, 'setItem')
    const sessionGet = vi.spyOn(window.sessionStorage, 'getItem')
    const sessionSet = vi.spyOn(window.sessionStorage, 'setItem')

    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)
    setActivePinia(createPinia())
    const { useSettingsStore } = await import('../stores/settings')
    const settings = useSettingsStore()

    await nextTick()
    expect(settings.isDark).toBe(false)
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(preferences.readPreference('woca-theme')).toBeNull()
    expect(setPreference).not.toHaveBeenCalled()

    settings.isDark = true
    await nextTick()
    expect(settings.isDark).toBe(true)
    expect(document.documentElement.classList.contains('dark')).toBe(true)

    await expect(preferences.drainPreferenceWrites()).rejects.toThrow('unable to persist native preferences')
    await nextTick()
    expect(settings.isDark).toBe(false)
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(preferences.readPreference('woca-theme')).toBeNull()
    expect(setPreference).toHaveBeenCalledTimes(1)

    expect(preferences.retryFailedNativePreference('woca-theme')).toBe(true)
    await nextTick()
    expect(settings.isDark).toBe(true)
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    await preferences.drainPreferenceWrites()
    expect(preferences.readPreference('woca-theme')).toBe('dark')
    expect(setPreference).toHaveBeenCalledTimes(2)
    expect(localGet).not.toHaveBeenCalled()
    expect(localSet).not.toHaveBeenCalled()
    expect(sessionGet).not.toHaveBeenCalled()
    expect(sessionSet).not.toHaveBeenCalled()
  })

  it('sends every write immediately and ignores an older failed completion', async () => {
    let failFirst!: () => void
    let finishSecond!: (result: PreferenceWriteResult) => void
    const setPreference = vi
      .fn<NativeAppBridge['SetPreference']>()
      .mockImplementationOnce(
        () => new Promise<PreferenceWriteResult>((_, reject) => (failFirst = () => reject(new Error('older failed')))),
      )
      .mockImplementationOnce(
        () => new Promise<PreferenceWriteResult>((resolve) => (finishSecond = resolve)),
      )
    const bridge = bridgeWithPreferences({ SetPreference: setPreference })
    vi.stubGlobal('go', { main: { App: bridge } })
    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)

    preferences.writeNativePreference('woca.pref.model', 'first')
    preferences.writeNativePreference('woca.pref.model', 'second')

    expect(setPreference).toHaveBeenCalledTimes(2)
    const secondInput = setPreference.mock.calls[1][0]
    expect(secondInput.revision).toBeGreaterThan(setPreference.mock.calls[0][0].revision)

    failFirst()
    await Promise.resolve()
    expect(preferences.readPreference('woca.pref.model')).toBe('second')

    finishSecond({ revision: secondInput.revision, applied: true })
    await preferences.drainPreferenceWrites()
    expect(preferences.readPreference('woca.pref.model')).toBe('second')
  })

  it('reconciles a late older success after the latest write already failed', async () => {
    let finishFirst!: (result: PreferenceWriteResult) => void
    let failSecond!: () => void
    const setPreference = vi
      .fn<NativeAppBridge['SetPreference']>()
      .mockImplementationOnce(
        () => new Promise<PreferenceWriteResult>((resolve) => (finishFirst = resolve)),
      )
      .mockImplementationOnce(
        () => new Promise<PreferenceWriteResult>((_, reject) => (failSecond = () => reject(new Error('latest failed')))),
      )
      .mockImplementation(async (input) => ({ revision: input.revision, applied: true }))
    const bridge = bridgeWithPreferences({ SetPreference: setPreference })
    vi.stubGlobal('go', { main: { App: bridge } })
    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)

    preferences.writeNativePreference('woca.pref.model', 'first')
    preferences.writeNativePreference('woca.pref.model', 'second')
    const firstRevision = setPreference.mock.calls[0][0].revision

    failSecond()
    await Promise.resolve()
    await Promise.resolve()
    expect(preferences.readPreference('woca.pref.model')).toBe('native-model')

    finishFirst({ revision: firstRevision, applied: true })
    await expect(preferences.drainPreferenceWrites()).rejects.toThrow('unable to persist native preferences')
    expect(preferences.readPreference('woca.pref.model')).toBe('first')

    expect(preferences.retryFailedNativePreference('woca.pref.model')).toBe(true)
    await preferences.drainPreferenceWrites()
    expect(preferences.readPreference('woca.pref.model')).toBe('second')
  })
})
