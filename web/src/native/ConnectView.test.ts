import { mount } from '@vue/test-utils'
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
  ])('does not submit an invalid origin: %s', async (_name, origin) => {
    const { connect, wrapper } = mountConnect()
    await wrapper.get('[data-testid="url-input"]').setValue(origin)
    await wrapper.get('[data-testid="api-key-input"]').setValue('woca_test_key')
    await wrapper.get('form').trigger('submit')

    expect(connect).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="connection-validation"]').text()).toBe(
      'Enter an absolute HTTPS origin without a path, query, fragment, or credentials. HTTP is allowed only for localhost, 127.0.0.1, or ::1.',
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
