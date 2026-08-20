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
    Preference: vi.fn(async (key: string) => ({
      value: storedNativePreferences[key] ?? '',
      found: key in storedNativePreferences,
    })),
    PutPreference: vi.fn(async () => undefined),
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

    expect(bridge.PutPreference).toHaveBeenCalledWith({ key: 'woca-theme', value: 'auto' })
    expect(bridge.PutPreference).toHaveBeenCalledWith({ key: 'woca.pref.locale', value: 'zh-CN' })
    expect(bridge.PutPreference).toHaveBeenCalledWith({ key: 'woca.pref.autoCollapseTools', value: 'true' })
    expect(bridge.PutPreference).toHaveBeenCalledWith({ key: 'woca.pref.markdownPreview', value: 'false' })
    expect(bridge.PutPreference).toHaveBeenCalledWith({ key: 'woca.pref.model', value: 'updated-native-model' })
    expect(bridge.PutPreference).toHaveBeenCalledWith({
      key: 'woca.pref.permissionMode',
      value: 'bypassPermissions',
    })
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

  it('serializes native writes so a stale completion cannot win', async () => {
    let finishFirst!: () => void
    const putPreference = vi
      .fn<NativeAppBridge['PutPreference']>()
      .mockImplementationOnce(() => new Promise<void>((resolve) => (finishFirst = resolve)))
      .mockResolvedValue(undefined)
    const bridge = bridgeWithPreferences({ PutPreference: putPreference })
    vi.stubGlobal('go', { main: { App: bridge } })
    const preferences = await import('./preferences')
    await preferences.hydrateNativePreferences(bridge)

    preferences.writeNativePreference('woca.pref.model', 'first')
    preferences.writeNativePreference('woca.pref.model', 'second')
    await Promise.resolve()

    expect(putPreference).toHaveBeenCalledTimes(1)
    finishFirst()
    await preferences.drainPreferenceWrites()
    expect(putPreference).toHaveBeenNthCalledWith(2, { key: 'woca.pref.model', value: 'second' })
  })
})
