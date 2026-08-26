import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'

import ConnectView from './ConnectView.vue'

function mountConnect(connect = vi.fn().mockResolvedValue({ state: 'ready' })) {
  return {
    connect,
    wrapper: mount(ConnectView, {
      props: { connect },
      global: { plugins: [i18n] },
    }),
  }
}

describe('ConnectView', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })

  it('requires both an enterprise URL and API key', async () => {
    const blankUrl = mountConnect()
    await blankUrl.wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await blankUrl.wrapper.get('form').trigger('submit')

    expect(blankUrl.connect).not.toHaveBeenCalled()
    expect(blankUrl.wrapper.get('[data-testid="connection-validation"]').text()).toContain('URL')

    const blankKey = mountConnect()
    await blankKey.wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await blankKey.wrapper.get('form').trigger('submit')

    expect(blankKey.connect).not.toHaveBeenCalled()
    expect(blankKey.wrapper.get('[data-testid="connection-validation"]').text()).toContain('API')
  })

  it.each([
    ['remote HTTP', 'http://enterprise.example.com'],
    ['relative URL', '/enterprise'],
    ['path', 'https://enterprise.example.com/app'],
    ['credentials', 'https://user:secret@enterprise.example.com'],
    ['query', 'https://enterprise.example.com?tenant=acme'],
    ['fragment', 'https://enterprise.example.com#login'],
    ['invalid port', 'https://enterprise.example.com:65536'],
    ['zero port', 'https://enterprise.example.com:0'],
    ['empty port', 'https://enterprise.example.com:'],
    ['empty userinfo', 'https://@enterprise.example.com'],
    ['encoded userinfo', 'https://%75ser@enterprise.example.com'],
    ['encoded at-sign userinfo', 'https://%40@enterprise.example.com'],
    ['backslash', 'https://enterprise.example.com\\'],
    ['tab control character', 'https://enterprise.\texample.com'],
    ['NUL control character', 'https://enterprise.example.com\u0000'],
    ['abbreviated IPv4 loopback', 'http://127.1'],
    ['decimal IPv4 loopback', 'http://2130706433'],
    ['hex IPv4 loopback', 'http://0x7f000001'],
    ['octal-like IPv4 loopback', 'http://0177.0.0.1'],
    ['bracketed non-IPv6 host', 'http://[localhost]:8080'],
    ['IPv6 zone', 'http://[fe80::1%25en0]:8080'],
    ['ambiguous authority', 'https://enterprise.example.com::443'],
    ['backtick host delimiter', 'https://exa`mple.com'],
    ['left brace host delimiter', 'https://exa{mple.com'],
    ['right brace host delimiter', 'https://exa}mple.com'],
    ['quote host delimiter', 'https://exa"mple.com'],
    ['left angle host delimiter', 'https://exa<mple.com'],
    ['right angle host delimiter', 'https://exa>mple.com'],
    ['caret host delimiter', 'https://exa^mple.com'],
    ['pipe host delimiter', 'https://exa|mple.com'],
    ['HTTPS abbreviated IPv4', 'https://127.1'],
    ['HTTPS decimal IPv4', 'https://2130706433'],
    ['HTTPS hexadecimal IPv4', 'https://0x7f000001'],
    ['HTTPS octal-like IPv4', 'https://0177.0.0.1'],
    ['soft hyphen in hostname', 'https://exa\u00admple.com'],
    ['zero-width space in hostname', 'https://exa\u200bmple.com'],
    ['BOM in hostname', 'https://exa\ufeffmple.com'],
    ['BOM inside authority port', 'https://enterprise.example.com:\ufeff8443'],
  ])('does not submit an invalid origin: %s', async (_name, origin) => {
    const { connect, wrapper } = mountConnect()
    await wrapper.get('[data-testid="url-input"]').setValue(origin)
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="connection-validation"]').text()).toBe(
      'Enter a URL like https://example.com (no path). HTTPS only, except localhost.',
    )
  })

  it.each([
    ['HTTPS origin', '  https://enterprise.example.com:8443/  ', 'https://enterprise.example.com:8443/'],
    ['loopback HTTP', 'http://localhost:8080', 'http://localhost:8080'],
  ])('submits a valid %s', async (_name, origin, want) => {
    const { connect, wrapper } = mountConnect()
    await wrapper.get('[data-testid="url-input"]').setValue(origin)
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).toHaveBeenCalledWith({ server_url: want, api_key: 'woca_test_key' })
  })

  it('submits URL and API key only to the provided callback', async () => {
    const { connect, wrapper } = mountConnect()
    await wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).toHaveBeenCalledOnce()
    expect(connect).toHaveBeenCalledWith({
      server_url: 'https://enterprise.example.com',
      api_key: 'woca_test_key',
    })
  })

  it('never writes the API key to localStorage or sessionStorage', async () => {
    const localStorageSetItem = vi.spyOn(window.localStorage, 'setItem')
    const sessionStorageSetItem = vi.spyOn(window.sessionStorage, 'setItem')
    const { wrapper } = mountConnect()
    await wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(localStorageSetItem).not.toHaveBeenCalled()
    expect(sessionStorageSetItem).not.toHaveBeenCalled()
  })

  it('clears the API key field after a failed connection', async () => {
    const { wrapper } = mountConnect(vi.fn().mockRejectedValue(new Error('activation failed')))
    await wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect((wrapper.get('[data-testid="url-input"]').element as HTMLInputElement).value).toBe(
      'https://enterprise.example.com',
    )
    expect((wrapper.get('[data-testid="api-key-input"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.get('[role="alert"]').text()).not.toContain('activation failed')
  })
})

describe('ConnectView three-entry model (design §6.6)', () => {
  beforeEach(() => {
    vi.stubGlobal('go', undefined)
    vi.unstubAllGlobals()
  })

  function nativeApp(edgeBridges?: () => Promise<EdgeBridge[]>) {
    return {
      Bootstrap: vi.fn(async () => ({ state: 'ready' as const })),
      Connect: vi.fn(async () => ({ state: 'ready' as const })),
      Logout: vi.fn(async () => ({ state: 'authorization_required' as const })),
      ConnectEmbedded: vi.fn(async () => ({ state: 'ready' as const })),
      EdgeBridges: edgeBridges ?? vi.fn(async () => [] as EdgeBridge[]),
      StreamCredentials: vi.fn(async () => ({ scheme: '', token: '' })),
      Preference: vi.fn(async () => ({ value: '', found: false })),
      SetPreference: vi.fn(async () => ({ revision: 1, applied: true })),
      CancelRequest: vi.fn(async () => true),
    }
  }

  it('shows the embedded entry only when an embedded bridge is running', async () => {
    const { wrapper } = mountConnect()
    expect(wrapper.find('[data-testid="embedded-entry"]').exists()).toBe(false)

    const withEmbedded = mount(ConnectView, {
      props: { connect: vi.fn().mockResolvedValue({ state: 'ready' }), embeddedUrl: 'http://127.0.0.1:39871' },
      global: { plugins: [i18n] },
    })
    expect(withEmbedded.find('[data-testid="embedded-entry"]').exists()).toBe(true)
    await withEmbedded.find('[data-testid="embedded-connect"]').trigger('click')
    expect(withEmbedded.emitted('connect-embedded')).toHaveLength(1)
  })

  it('switches URL label and placeholder between the private-cloud and direct tabs', async () => {
    const { wrapper } = mountConnect()
    expect(wrapper.get('[data-testid="tab-private"]').attributes('aria-selected')).toBe('true')
    expect(wrapper.get('[data-testid="url-input"]').attributes('placeholder')).toBe('https://woca.example.com')

    await wrapper.get('[data-testid="tab-direct"]').trigger('click')
    expect(wrapper.get('[data-testid="tab-direct"]').attributes('aria-selected')).toBe('true')
    expect(wrapper.get('[data-testid="url-input"]').attributes('placeholder')).toBe('https://bridge.example.com')
  })

  it('asks the edge for its bridges on the private tab and shows a selection step when several are registered', async () => {
    const app = nativeApp(vi.fn(async () => [
      { bridge_id: 'br_one', created_at: 1, last_seen: 2, live: true },
      { bridge_id: 'br_two', created_at: 1, last_seen: 2, live: false },
    ]))
    vi.stubGlobal('go', { main: { App: app } })
    const connect = vi.fn().mockResolvedValue({ state: 'ready' })
    const { wrapper } = mountConnect(connect)
    await wrapper.get('[data-testid="url-input"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(app.EdgeBridges).toHaveBeenCalledWith('https://edge.example.com')
    expect(connect).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="bridge-step"]').exists()).toBe(true)

    const buttons = wrapper.findAll('.gate-bridge')
    expect(buttons).toHaveLength(2)
    await buttons[1].trigger('click')
    await flushPromises()
    expect(connect).toHaveBeenCalledWith({
      server_url: 'https://edge.example.com',
      api_key: 'woca_test_key',
      bridge_id: 'br_two',
    })
  })

  it('reports an error when the edge has no registered bridges', async () => {
    const app = nativeApp()
    vi.stubGlobal('go', { main: { App: app } })
    const connect = vi.fn().mockResolvedValue({ state: 'ready' })
    const { wrapper } = mountConnect(connect)
    await wrapper.get('[data-testid="url-input"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('No bridges found. Check the address, or contact your administrator.')
  })

  it('skips the inventory when the URL carries a direct bridge link', async () => {
    const app = nativeApp()
    vi.stubGlobal('go', { main: { App: app } })
    const connect = vi.fn().mockResolvedValue({ state: 'ready' })
    const { wrapper } = mountConnect(connect)
    await wrapper.get('[data-testid="url-input"]').setValue('https://edge.example.com/?bridge=br_pinned')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(app.EdgeBridges).not.toHaveBeenCalled()
    expect(connect).toHaveBeenCalledWith({
      server_url: 'https://edge.example.com/',
      api_key: 'woca_test_key',
      bridge_id: 'br_pinned',
    })
  })
})
