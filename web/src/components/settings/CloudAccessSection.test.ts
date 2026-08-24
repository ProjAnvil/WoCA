import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getCloudAccess: vi.fn(),
  updateCloudAccess: vi.fn(),
  resetCloudAccess: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import CloudAccessSection from './CloudAccessSection.vue'

function snapshot(over: Record<string, unknown> = {}) {
  return {
    url: 'https://edge.example.com',
    url_source: 'settings',
    token_source: 'settings',
    token_set: true,
    enrolled: true,
    stale: false,
    spki_pinned: true,
    tunnel: { serving: true, served: 0 },
    ...over,
  }
}

function mountSection() {
  return mount(CloudAccessSection, { global: { plugins: [i18n] } })
}

async function field(w: ReturnType<typeof mountSection>, selector: string) {
  await flushPromises()
  return w.get(selector)
}

describe('CloudAccessSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    for (const fn of Object.values(api)) fn.mockReset()
  })

  it('loads the snapshot and saves trimmed edge URL + token', async () => {
    api.getCloudAccess.mockResolvedValue(snapshot())
    api.updateCloudAccess.mockResolvedValue(snapshot({ url: 'https://edge2.example.com' }))

    const w = mountSection()
    const url = await field(w, 'input[data-testid="cloud-url"]')
    expect((url.element as HTMLInputElement).value).toBe('https://edge.example.com')

    await url.setValue('  https://edge2.example.com  ')
    const token = await field(w, 'input[data-testid="cloud-token"]')
    await token.setValue('  woca_enroll_x  ')
    const save = await field(w, '[data-testid="cloud-save"]')
    ;(save.element as HTMLButtonElement).click()
    await flushPromises()

    expect(api.updateCloudAccess).toHaveBeenCalledWith('https://edge2.example.com', 'woca_enroll_x')
  })

  it('labels an env-pinned URL as read-only posture', async () => {
    api.getCloudAccess.mockResolvedValue(snapshot({ url_source: 'env' }))
    const w = mountSection()
    await flushPromises()
    expect(w.text()).toContain('environment variable')
  })

  it('surfaces a failed save with the server message', async () => {
    api.getCloudAccess.mockResolvedValue(snapshot())
    api.updateCloudAccess.mockRejectedValue(new Error('conflict'))
    const w = mountSection()
    await flushPromises()

    const url = await field(w, 'input[data-testid="cloud-url"]')
    await url.setValue('https://other.example.com')
    const save = await field(w, '[data-testid="cloud-save"]')
    ;(save.element as HTMLButtonElement).click()
    await flushPromises()
    expect(w.text()).toContain('conflict')
  })
})
