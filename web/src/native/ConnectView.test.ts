import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
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
