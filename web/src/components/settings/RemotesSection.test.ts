import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getWebProfiles: vi.fn(),
  addWebProfile: vi.fn(),
  useWebProfile: vi.fn(),
  deleteWebProfile: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import RemotesSection from './RemotesSection.vue'

function mountSection() {
  const pinia = createPinia()
  setActivePinia(pinia)
  return mount(RemotesSection, { global: { plugins: [i18n, pinia] } })
}

describe('RemotesSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getWebProfiles.mockReset().mockResolvedValue({
      profiles: [
        { id: 'p_a', url: 'https://edge.example.com', label: 'Cloud', added_at: 1 },
        { id: 'p_b', url: 'http://10.0.0.5:3490', added_at: 2 },
      ],
      last: 'p_a',
    })
    api.addWebProfile.mockReset().mockResolvedValue({ id: 'p_c', url: 'https://x', added_at: 3 })
    api.useWebProfile.mockReset().mockResolvedValue({})
    api.deleteWebProfile.mockReset().mockResolvedValue({ profiles: [], last: '' })
  })

  it('lists saved remotes and marks the last-used one', async () => {
    const w = mountSection()
    await flushPromises()
    const items = w.findAll('[data-testid^="remote-profile-"]')
    expect(items).toHaveLength(2)
    expect(w.text()).toContain('Cloud')
    expect(w.text()).toContain('http://10.0.0.5:3490')
    expect(w.text()).toContain('Last used')
  })

  it('picking a remote marks it used and navigates', async () => {
    // jsdom's location is read-only; replace it once with an observable
    // stub so the component's full-page navigation is assertable.
    const stub = { href: 'http://localhost:3490/' as string }
    Object.defineProperty(window, 'location', { value: stub, writable: true, configurable: true })

    const w = mountSection()
    await flushPromises()
    await w.find('[data-testid="remote-profile-p_b"]').trigger('click')
    await flushPromises()
    expect(api.useWebProfile).toHaveBeenCalledWith('p_b')
    expect(stub.href).toBe('http://10.0.0.5:3490')
  })

  it('adds a remote and refreshes the list', async () => {
    const w = mountSection()
    await flushPromises()
    await w.find('input').setValue('https://edge.two.com')
    await w.findAll('button.btn').at(-1)!.trigger('click')
    await flushPromises()
    expect(api.addWebProfile).toHaveBeenCalledWith('https://edge.two.com', '')
  })

  it('deletes a remote', async () => {
    const w = mountSection()
    await flushPromises()
    await w.find('[data-testid="remote-remove-p_b"]').trigger('click')
    await flushPromises()
    expect(api.deleteWebProfile).toHaveBeenCalledWith('p_b')
  })
})
