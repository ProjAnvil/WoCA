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
  props: ['serverUrl'],
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

  it('mounts the app after native state becomes ready', async () => {
    getNativeBridge.mockReturnValue(nativeBridge(async () => ({ state: 'ready' })))

    const wrapper = mountGate()
    await flushPromises()

    expect(wrapper.get('[data-testid="app-marker"]').text()).toBe('Woca app')
    expect(wrapper.find('[data-testid="connect-view"]').exists()).toBe(false)
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
})
