import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import type { NativeBridge } from './bridge'

const { getNativeBridge } = vi.hoisted(() => ({
  getNativeBridge: vi.fn(),
}))

vi.mock('./bridge', () => ({ getNativeBridge }))

import NativeGate from './NativeGate.vue'

const connectViewStub = {
  name: 'ConnectView',
  props: ['serverUrl', 'connect'],
  template: '<div data-testid="connect-view">{{ serverUrl }}</div>',
}

function mountGate() {
  return mount(NativeGate, {
    slots: { default: '<p data-testid="app-marker">Woca app</p>' },
    global: { plugins: [i18n], stubs: { ConnectView: connectViewStub } },
  })
}

function nativeBridge(bootstrap: NativeBridge['Bootstrap'], theme?: string): NativeBridge {
  return {
    Bootstrap: bootstrap,
    Connect: vi.fn(),
    ConnectEmbedded: vi.fn(),
    EdgeBridges: vi.fn(async () => []),
    StreamCredentials: vi.fn(async () => ({ scheme: '', token: '' })),
    Logout: vi.fn(),
    Preference: vi.fn(async (key?: string) =>
      key === 'woca-theme' && theme !== undefined
        ? { value: theme, found: true }
        : { value: '', found: false },
    ),
    SetPreference: vi.fn(async (input) => ({ revision: input.revision, applied: true })),
    CancelRequest: vi.fn(async () => true),
  }
}

describe('NativeGate', () => {
  beforeEach(() => {
    getNativeBridge.mockReset()
    i18n.global.locale.value = 'en'
    document.documentElement.classList.remove('dark')
  })

  it('renders the app immediately in a normal browser', () => {
    getNativeBridge.mockReturnValue(null)

    const wrapper = mountGate()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
  })

  it('does not mount the app before native bootstrap resolves', () => {
    let resolveBootstrap!: (state: BootstrapState) => void
    getNativeBridge.mockReturnValue(
      nativeBridge(
        () =>
          new Promise((resolve) => {
            resolveBootstrap = resolve
          }),
      ),
    )

    const wrapper = mountGate()

    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="native-loading"]').text()).toBeTruthy()

    resolveBootstrap({ state: 'ready' })
  })

  it('shows the connection view for an unconfigured native client', async () => {
    getNativeBridge.mockReturnValue(nativeBridge(async () => ({ state: 'unconfigured' })))

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.get('[data-testid="connect-view"]').attributes('data-testid')).toBe('connect-view')
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
  })

  it('applies the hydrated dark theme to the connection gate', async () => {
    getNativeBridge.mockReturnValue(nativeBridge(async () => ({ state: 'unconfigured' }), 'dark'))

    mountGate()
    await flushPromises()

    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('keeps the connection gate light when the hydrated theme is light', async () => {
    getNativeBridge.mockReturnValue(nativeBridge(async () => ({ state: 'unconfigured' }), 'light'))

    mountGate()
    await flushPromises()

    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })

  it('withholds the app until ready preferences finish hydrating', async () => {
    let finishHydration!: (value: PreferenceValue) => void
    const bridge = nativeBridge(async () => ({ state: 'ready' }))
    const hydration = new Promise<PreferenceValue>((resolve) => (finishHydration = resolve))
    bridge.Preference = vi.fn(() => hydration)
    getNativeBridge.mockReturnValue(bridge)

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="native-loading"]').exists()).toBe(true)

    finishHydration({ value: '', found: false })
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
    expect(wrapper.find('[data-testid="connect-view"]').exists()).toBe(false)
  })

  it('applies the hydrated locale before mounting the ready app', async () => {
    const bridge = nativeBridge(async () => ({ state: 'ready' }))
    bridge.Preference = vi.fn(async (key) =>
      key === 'woca.pref.locale' ? { value: 'zh-CN', found: true } : { value: '', found: false },
    )
    getNativeBridge.mockReturnValue(bridge)

    const wrapper = mountGate()
    await flushPromises()

    expect(i18n.global.locale.value).toBe('zh-CN')
    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
  })

  it('withholds the app while preferences hydrate after Connect becomes ready', async () => {
    let finishHydration!: (value: PreferenceValue) => void
    const bridge = nativeBridge(async () => ({ state: 'unconfigured' }))
    bridge.Connect = vi.fn<NativeBridge['Connect']>().mockResolvedValue({ state: 'ready' })
    const hydration = new Promise<PreferenceValue>((resolve) => (finishHydration = resolve))
    bridge.Preference = vi.fn(() => hydration)
    getNativeBridge.mockReturnValue(bridge)

    const wrapper = mountGate()
    await flushPromises()
    const connect = wrapper.findComponent(connectViewStub).props('connect') as (
      input: ConnectionInput,
    ) => Promise<void>
    const connecting = connect({ server_url: 'https://enterprise.example.com', api_key: 'ephemeral' })
    await flushPromises()

    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="native-loading"]').exists()).toBe(true)

    finishHydration({ value: '', found: false })
    await connecting
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
  })

  it('degrades to preference defaults when hydration fails, never trapping the gate', async () => {
    const bridge = nativeBridge(async () => ({ state: 'ready' }))
    bridge.Preference = vi
      .fn<NativeBridge['Preference']>()
      .mockRejectedValue(new Error('stored secret: woca_test_key'))
    getNativeBridge.mockReturnValue(bridge)

    const wrapper = mountGate()
    await flushPromises()

    // A preference read failure is cosmetic: the session proceeds and the
    // raw error (which may echo key material) never reaches the DOM.
    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
    expect(wrapper.text()).not.toContain('woca_test_key')
  })

  it('keeps the server URL when authorization is required', async () => {
    getNativeBridge.mockReturnValue(
      nativeBridge(async () => ({
        state: 'authorization_required',
        server_url: 'https://enterprise.example.com',
      })),
    )

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.get('[data-testid="connect-view"]').text()).toBe('https://enterprise.example.com')
  })

  it('shows a sanitized retryable error when bootstrap fails', async () => {
    const bootstrap = vi
      .fn<NativeBridge['Bootstrap']>()
      .mockRejectedValueOnce(new Error('server secret: woca_test_key'))
      .mockResolvedValueOnce({ state: 'ready' })
    getNativeBridge.mockReturnValue(nativeBridge(bootstrap))

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).not.toContain('woca_test_key')
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)

    i18n.global.locale.value = 'zh-CN'
    await flushPromises()
    expect(wrapper.get('button').text()).toBe(i18n.global.t('native.retry'))

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
  })

  it('always offers a way back to the connect page from the error screen', async () => {
    // Bootstrap keeps failing: retry must not become a dead-end loop —
    // the second button returns to the connect page regardless.
    getNativeBridge.mockReturnValue(
      nativeBridge(
        vi.fn<NativeBridge['Bootstrap']>().mockRejectedValue(new Error('binding down')),
      ),
    )

    const wrapper = mountGate()
    await flushPromises()

    const buttons = wrapper.findAll('button')
    expect(buttons.length).toBe(2)
    expect(buttons[0].text()).toBe('Retry')
    expect(buttons[1].text()).toBe('Back to connect page')

    await buttons[1].trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="connect-view"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
  })

  it('shows a localized fatal local-data screen without mounting app or connection UI', async () => {
    getNativeBridge.mockReturnValue(
      nativeBridge(async () => ({
        state: 'fatal_local_data',
        database_path: '/Users/test/Library/Application Support/woca-native-client/client.db',
        message: 'raw SQLite error: secret schema detail',
      })),
    )

    const wrapper = mountGate()
    await flushPromises()

    const fatal = wrapper.get('[data-testid="fatal-local-data"]')
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="connect-view"]').exists()).toBe(false)
    expect(fatal.get('code').text()).toBe(
      '/Users/test/Library/Application Support/woca-native-client/client.db',
    )
    expect(fatal.text()).toContain("woca can't read its local configuration.")
    expect(fatal.text()).toContain('Check the file permissions and relaunch woca. If it keeps failing, contact your administrator with the path below.')
    expect(fatal.text()).not.toContain('secret schema detail')
    expect(fatal.find('button').exists()).toBe(false)

    i18n.global.locale.value = 'zh-CN'
    await flushPromises()
    expect(fatal.text()).toContain(i18n.global.t('native.localDataFatalDescription'))
    expect(fatal.text()).toContain(i18n.global.t('native.localDataFatalRecovery'))
  })

  it('uses fixed unavailable-path copy when the database path cannot be resolved', async () => {
    getNativeBridge.mockReturnValue(
      nativeBridge(async () => ({
        state: 'fatal_local_data',
        message: 'raw home directory failure',
      })),
    )

    const wrapper = mountGate()
    await flushPromises()

    const fatal = wrapper.get('[data-testid="fatal-local-data"]')
    expect(fatal.get('code').text()).toBe('Unavailable')
    expect(fatal.text()).not.toContain('raw home directory failure')
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="connect-view"]').exists()).toBe(false)
  })
})
