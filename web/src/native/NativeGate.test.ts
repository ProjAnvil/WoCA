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

function nativeBridge(bootstrap: NativeBridge['Bootstrap']): NativeBridge {
  return {
    Bootstrap: bootstrap,
    Connect: vi.fn(),
    Logout: vi.fn(),
    Preference: vi.fn(async () => ({ value: '', found: false })),
    SetPreference: vi.fn(async (input) => ({ revision: input.revision, applied: true })),
    CancelRequest: vi.fn(async () => true),
  }
}

describe('NativeGate', () => {
  beforeEach(() => {
    getNativeBridge.mockReset()
    i18n.global.locale.value = 'en'
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

  it('keeps the app gated with a sanitized retry when preference hydration fails', async () => {
    const bridge = nativeBridge(async () => ({ state: 'ready' }))
    bridge.Preference = vi
      .fn<NativeBridge['Preference']>()
      .mockRejectedValueOnce(new Error('stored secret: woca_test_key'))
      .mockResolvedValue({ value: '', found: false })
    getNativeBridge.mockReturnValue(bridge)

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).not.toContain('woca_test_key')
    expect(wrapper.find('[data-testid="app-marker"]').exists()).toBe(false)

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
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
    expect(wrapper.get('button').text()).toBe('重试')

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
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
    expect(fatal.text()).toContain('Woca cannot open its local configuration database.')
    expect(fatal.text()).toContain('Fix the file permissions or database schema, then relaunch Woca.')
    expect(fatal.text()).not.toContain('secret schema detail')
    expect(fatal.find('button').exists()).toBe(false)

    i18n.global.locale.value = 'zh-CN'
    await flushPromises()
    expect(fatal.text()).toContain('Woca 无法打开本地配置数据库。')
    expect(fatal.text()).toContain('修复文件权限或数据库结构后，请重新启动 Woca。')
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
