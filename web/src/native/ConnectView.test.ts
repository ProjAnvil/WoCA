import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'

import ConnectView from './ConnectView.vue'
import type { EnrollEmbeddedInput, EnrollResult } from './bridge'

function mountConnect(connect = vi.fn().mockResolvedValue({ state: 'ready' })) {
  return {
    connect,
    wrapper: mount(ConnectView, {
      props: { connect },
      global: { plugins: [i18n] },
    }),
  }
}

// The wizard lands on the mode-choice page; form tests walk P0 -> P1 ->
// the requested form page first.
async function openForm(
  wrapper: ReturnType<typeof mountConnect>['wrapper'],
  flavor: 'private' | 'direct' = 'private',
) {
  await wrapper.get('[data-testid="remote-open"]').trigger('click')
  await wrapper.get('[data-testid="tab-' + flavor + '"]').trigger('click')
}

describe('ConnectView', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })

  it('requires both an enterprise URL and API key', async () => {
    const blankUrl = mountConnect()
    await openForm(blankUrl.wrapper)
    await blankUrl.wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await blankUrl.wrapper.get('form').trigger('submit')

    expect(blankUrl.connect).not.toHaveBeenCalled()
    expect(blankUrl.wrapper.get('[data-testid="connection-validation"]').text()).toContain('URL')

    const blankKey = mountConnect()
    await openForm(blankKey.wrapper)
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
    await openForm(wrapper)
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
    await openForm(wrapper)
    await wrapper.get('[data-testid="url-input"]').setValue(origin)
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).toHaveBeenCalledWith({ server_url: want, api_key: 'woca_test_key' })
  })

  it('submits URL and API key only to the provided callback', async () => {
    const { connect, wrapper } = mountConnect()
    await openForm(wrapper)
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
    await openForm(wrapper)
    await wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(localStorageSetItem).not.toHaveBeenCalled()
    expect(sessionStorageSetItem).not.toHaveBeenCalled()
  })

  it('keeps the API key in the field after a failed connection', async () => {
    const { wrapper } = mountConnect(vi.fn().mockRejectedValue(new Error('activation failed')))
    await openForm(wrapper)
    await wrapper.get('[data-testid="url-input"]').setValue('https://enterprise.example.com')
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect((wrapper.get('[data-testid="url-input"]').element as HTMLInputElement).value).toBe(
      'https://enterprise.example.com',
    )
    // A failed attempt must not force a retype: the key stays in memory
    // (never persisted) until a successful connect clears it.
    expect((wrapper.get('[data-testid="api-key-input"]').element as HTMLInputElement).value).toBe(
      'woca_test_key',
    )
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

  it('walks P0, P1 and the private-cloud / direct form pages', async () => {
    const { wrapper } = mountConnect()
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    await wrapper.get('[data-testid="tab-private"]').trigger('click')
    expect(wrapper.get('[data-testid="url-input"]').attributes('placeholder')).toBe('https://woca.example.com')

    // Back to the remote-way page, then the direct form.
    await wrapper.get('.gate-back-btn').trigger('click')
    await wrapper.get('[data-testid="tab-direct"]').trigger('click')
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
    await openForm(wrapper)
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
    await openForm(wrapper)
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
    await openForm(wrapper)
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

  // The saved-connection list (one-tap resume): profiles with a live
  // session continue directly; expired ones prefill the key form.
  function profileListApp() {
    return {
      ...nativeApp(),
      Profiles: vi.fn(async () => [
        { id: 7, server_url: 'https://edge.example.com', bridge_id: 'br_live', session_state: 'active' as const, last_used_at: 2, active: true },
        { id: 8, server_url: 'https://other.example.com', session_state: 'expired' as const, last_used_at: 1, active: false },
      ]),
      ResumeProfile: vi.fn(async () => ({ state: 'ready' as const })),
      DeleteProfile: vi.fn(async () => ({ deleted: true, had_session: true, revoked: true })),
    }
  }

  it('lists saved connections and resumes a signed-in profile in one tap', async () => {
    const app = profileListApp()
    vi.stubGlobal('go', { main: { App: app } })
    const resume = vi.fn().mockResolvedValue({ state: 'ready' })
    const wrapper = mount(ConnectView, {
      props: { connect: vi.fn().mockResolvedValue({ state: 'ready' }), resume },
      global: { plugins: [i18n] },
    })
    await flushPromises()

    const list = wrapper.get('[data-testid="saved-profiles"]')
    expect(list.text()).toContain('edge.example.com')
    expect(list.text()).toContain('Signed in')
    expect(list.text()).toContain('Last used')

    const rows = wrapper.findAll('.gate-profile-main')
    expect(rows).toHaveLength(2)
    await rows[0].trigger('click')
    await flushPromises()
    expect(resume).toHaveBeenCalledWith(7)
  })

  it('prefills the key form when picking a profile whose session expired', async () => {
    const app = profileListApp()
    vi.stubGlobal('go', { main: { App: app } })
    const resume = vi.fn().mockResolvedValue({ state: 'ready' })
    const wrapper = mount(ConnectView, {
      props: { connect: vi.fn().mockResolvedValue({ state: 'ready' }), resume },
      global: { plugins: [i18n] },
    })
    await flushPromises()

    const rows = wrapper.findAll('.gate-profile-main')
    await rows[1].trigger('click')
    await flushPromises()

    expect(resume).not.toHaveBeenCalled()
    expect((wrapper.get('[data-testid="url-input"]').element as HTMLInputElement).value).toBe(
      'https://other.example.com',
    )
  })

  it('deletes a saved connection after an inline confirmation and flags a failed revocation', async () => {
    const app = profileListApp()
    app.DeleteProfile = vi.fn(async () => ({ deleted: true, had_session: true, revoked: false }))
    vi.stubGlobal('go', { main: { App: app } })
    const { wrapper } = mountConnect()
    await flushPromises()

    expect(wrapper.find('[data-testid="profile-delete-confirm"]').exists()).toBe(false)
    await wrapper.findAll('.gate-profile-delete')[0].trigger('click')
    expect(app.DeleteProfile).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="profile-delete-confirm"]').exists()).toBe(true)

    await wrapper.get('.gate-profile-confirm-delete').trigger('click')
    await flushPromises()
    expect(app.DeleteProfile).toHaveBeenCalledWith(7)
    expect(wrapper.findAll('.gate-profile-main')).toHaveLength(1)
    // The local deletion went through; the residual server session is
    // reported instead of being papered over.
    expect(wrapper.get('.gate-profile-note').text()).toContain(
      'signing out the server session failed',
    )
  })
})

describe('ConnectView embedded-bridge enrollment (three-field registration)', () => {
  beforeEach(() => {
    vi.stubGlobal('go', undefined)
    vi.unstubAllGlobals()
    i18n.global.locale.value = 'en'
  })

  interface EnrollMountOpts {
    enrolled?: boolean
    bridgeId?: string
    edgeURL?: string
    enroll?: (input: EnrollEmbeddedInput) => Promise<EnrollResult>
    deregister?: () => Promise<EnrollResult>
    connect?: (input: ConnectionInput) => Promise<unknown>
  }

  function enrollApp(o: EnrollMountOpts) {
    return {
      Bootstrap: vi.fn(async () => ({ state: 'ready' as const })),
      Connect: vi.fn(async () => ({ state: 'ready' as const })),
      Logout: vi.fn(async () => ({ state: 'authorization_required' as const })),
      ConnectEmbedded: vi.fn(async () => ({ state: 'ready' as const })),
      EdgeBridges: vi.fn(async () => [] as EdgeBridge[]),
      StreamCredentials: vi.fn(async () => ({ scheme: '', token: '' })),
      Preference: vi.fn(async () => ({ value: '', found: false })),
      SetPreference: vi.fn(async () => ({ revision: 1, applied: true })),
      CancelRequest: vi.fn(async () => true),
      EmbeddedCloudStatus: vi.fn(async () => ({
        available: true,
        enrolled: Boolean(o.enrolled),
        bridge_id: o.bridgeId ?? 'br_emb',
        edge_url: o.edgeURL ?? 'https://edge.example.com',
      })),
      EnrollEmbedded:
        o.enroll ??
        vi.fn(async (): Promise<EnrollResult> => ({ state: { state: 'ready' }, error: '' })),
      DeregisterEmbedded: o.deregister ?? vi.fn(async () => ({ state: { state: 'unconfigured' as const }, error: '' })),
    }
  }

  function mountEnroll(o: EnrollMountOpts = {}) {
    const connect = o.connect ?? vi.fn().mockResolvedValue({ state: 'ready' })
    const app = enrollApp(o)
    vi.stubGlobal('go', { main: { App: app } })
    const wrapper = mount(ConnectView, {
      props: {
        connect,
        embeddedUrl: 'http://127.0.0.1:39871',
        enroll: o.enroll,
        deregister: o.deregister,
      },
      global: { plugins: [i18n] },
    })
    return { app, connect, wrapper }
  }

  async function openEnrollForm(wrapper: ReturnType<typeof mount>) {
    await flushPromises()
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    // out-in transition: wait for the leaving page before the next query.
    await flushPromises()
    await wrapper.get('[data-testid="embedded-enroll-open"]').trigger('click')
  }

  it('shows the registration link on the remote page for an unenrolled embedded bridge', async () => {
    const { wrapper } = mountEnroll()
    await flushPromises()
    // Landing page: the local card is the mode shape, no register/deregister.
    expect(wrapper.find('[data-testid="embedded-enroll-open"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="embedded-signin"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="embedded-deregister-arm"]').exists()).toBe(false)
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    expect(wrapper.find('[data-testid="embedded-enroll-open"]').exists()).toBe(true)
  })

  it('shows the enrolled badge with sign-in instead of the zero-input local entry', async () => {
    const { wrapper } = mountEnroll({ enrolled: true, bridgeId: 'br_prod' })
    await flushPromises()
    expect(wrapper.find('[data-testid="embedded-connect"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="embedded-enroll-open"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="embedded-entry"]').text()).toContain('br_prod')
    expect(wrapper.find('[data-testid="embedded-signin"]').exists()).toBe(true)
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    expect(wrapper.find('[data-testid="embedded-deregister-arm"]').exists()).toBe(true)
  })

  it('validates all three fields before calling the binding', async () => {
    const enroll = vi.fn(async (): Promise<EnrollResult> => ({ state: { state: 'ready' }, error: '' }))
    const { wrapper } = mountEnroll({ enroll })
    await openEnrollForm(wrapper)

    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    expect(wrapper.get('[data-testid="enroll-validation"]').text()).toContain('enrollment token')
    expect(enroll).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="enroll-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_user_key_wrong_shape')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    expect(wrapper.get('[data-testid="enroll-validation"]').text()).toContain('woca_enroll_')
    expect(enroll).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com/app')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_enroll_secret')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    expect(wrapper.get('[data-testid="enroll-validation"]').text()).toContain('no path')
    expect(enroll).not.toHaveBeenCalled()
  })

  it('submits the three fields exactly once and clears secrets on success', async () => {
    const enroll = vi.fn(async (): Promise<EnrollResult> => ({ state: { state: 'ready' }, error: '' }))
    const { wrapper } = mountEnroll({ enroll })
    await openEnrollForm(wrapper)
    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_enroll_secret')
    await wrapper.get('[data-testid="enroll-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    await flushPromises()

    expect(enroll).toHaveBeenCalledTimes(1)
    expect(enroll).toHaveBeenCalledWith({
      edge_url: 'https://edge.example.com',
      enrollment_token: 'woca_enroll_secret',
      api_key: 'woca_test_key',
    })
    expect((wrapper.get('[data-testid="enroll-token"]').element as HTMLInputElement).value).toBe('')
    expect((wrapper.get('[data-testid="enroll-key"]').element as HTMLInputElement).value).toBe('')
  })

  it.each([
    ['edge_unreachable', 'private cloud is unreachable', 'enroll-step'],
    ['enroll_token', 'invalid, expired, or already used', 'enroll-step'],
    ['bridge_write', 'write the enrollment settings', 'enroll-step'],
    ['bridge_restart', 'cloud posture', 'enroll-step'],
  ])('reports a terminal %s failure on the three-field form', async (cls, copy, step) => {
    const enroll = vi.fn(async () => ({ error: cls as EnrollResult['error'] }))
    const { wrapper } = mountEnroll({ enroll })
    await openEnrollForm(wrapper)
    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_enroll_secret')
    await wrapper.get('[data-testid="enroll-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[data-testid="enroll-error"]').text()).toContain(copy)
    expect(wrapper.find(`[data-testid="${step}"]`).exists()).toBe(true)
    // The values stay for the retry (the token field is masked).
    expect((wrapper.get('[data-testid="enroll-token"]').element as HTMLInputElement).value).toBe('woca_enroll_secret')
  })

  it('routes a rejected key to the single-field sign-in — the machine stays enrolled', async () => {
    const enroll = vi.fn(async () => ({
      state: { state: 'authorization_required' as const },
      bridge_id: 'br_emb',
      error: 'user_key' as const,
    }))
    const { wrapper } = mountEnroll({ enroll })
    await openEnrollForm(wrapper)
    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_enroll_secret')
    await wrapper.get('[data-testid="enroll-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    await flushPromises()

    // The sign-in view carries the failure copy, no token field exists,
    // and the typed key is prefilled for correction.
    expect(wrapper.find('[data-testid="enroll-login-step"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="enroll-login-error"]').text()).toContain('API key was rejected')
    expect(wrapper.find('[data-testid="enroll-token"]').exists()).toBe(false)
    expect((wrapper.get('[data-testid="enroll-login-key"]').element as HTMLInputElement).value).toBe('woca_test_key')
  })

  it('signs an enrolled bridge in through the standard connect path', async () => {
    const connect = vi.fn().mockResolvedValue({ state: 'ready' })
    const { wrapper } = mountEnroll({ enrolled: true, connect })
    await flushPromises()
    await wrapper.get('[data-testid="embedded-signin"]').trigger('click')
    await wrapper.get('[data-testid="enroll-login-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-login-submit"]').trigger('submit')
    await flushPromises()

    expect(connect).toHaveBeenCalledWith({
      server_url: 'https://edge.example.com',
      api_key: 'woca_test_key',
      bridge_id: 'br_emb',
    })
  })

  it('keeps the key after a failed sign-in', async () => {
    const connect = vi.fn().mockRejectedValue(new Error('activation failed'))
    const { wrapper } = mountEnroll({ enrolled: true, connect })
    await flushPromises()
    await wrapper.get('[data-testid="embedded-signin"]').trigger('click')
    await wrapper.get('[data-testid="enroll-login-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-login-submit"]').trigger('submit')
    await flushPromises()

    expect((wrapper.get('[data-testid="enroll-login-key"]').element as HTMLInputElement).value).toBe('woca_test_key')
    expect(wrapper.get('[data-testid="enroll-login-error"]').text()).toContain("Couldn't connect")
  })

  it('deregisters behind a two-step confirm and returns to the unenrolled page', async () => {
    const deregister = vi.fn(async (): Promise<EnrollResult> => ({ state: { state: 'unconfigured' }, error: '' }))
    const { wrapper } = mountEnroll({ enrolled: true, deregister })
    await flushPromises()
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    await wrapper.get('[data-testid="embedded-deregister-arm"]').trigger('click')
    expect(wrapper.find('[data-testid="embedded-deregister-confirm"]').exists()).toBe(true)
    expect(deregister).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="embedded-deregister-confirm-btn"]').trigger('click')
    await flushPromises()
    expect(deregister).toHaveBeenCalledTimes(1)

    // Back on the landing page the local card is unenrolled again; the
    // register link reappears on the remote page.
    expect(wrapper.find('[data-testid="embedded-signin"]').exists()).toBe(false)
    await wrapper.get('[data-testid="remote-open"]').trigger('click')
    expect(wrapper.find('[data-testid="embedded-enroll-open"]').exists()).toBe(true)
  })

  it('never writes the enrollment token or key to web storage', async () => {
    const ls = vi.spyOn(window.localStorage, 'setItem')
    const ss = vi.spyOn(window.sessionStorage, 'setItem')
    const enroll = vi.fn(async () => ({ error: 'enroll_token' as const }))
    const { wrapper } = mountEnroll({ enroll })
    await openEnrollForm(wrapper)
    await wrapper.get('[data-testid="enroll-url"]').setValue('https://edge.example.com')
    await wrapper.get('[data-testid="enroll-token"]').setValue('woca_enroll_secret')
    await wrapper.get('[data-testid="enroll-key"]').setValue('woca_test_key')
    await wrapper.get('[data-testid="enroll-submit"]').trigger('submit')
    await flushPromises()

    expect(ls).not.toHaveBeenCalled()
    expect(ss).not.toHaveBeenCalled()
  })
})
