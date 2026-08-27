import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../i18n'

const api = vi.hoisted(() => ({
  getStatus: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import Sidebar from './Sidebar.vue'
import type { BridgeStatus } from '../api/client'

function bridgeStatus(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    edition: 'bridge',
    managed: true,
    cloud: { enrolled: true, stale: false } as never,
    ...over,
  } as BridgeStatus
}

let w: ReturnType<typeof mount> | null = null

function mountSidebar() {
  const pinia = createPinia()
  setActivePinia(pinia)
  w = mount(Sidebar, { global: { plugins: [i18n, pinia] } })
  return w
}

function footerLabels(): string[] {
  return (w?.findAll('.footer-settings-btn') ?? []).map((b) =>
    b.text().replace(/\s+/g, ' ').trim()
  )
}

describe('Sidebar footer', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset()
  })
  afterEach(() => {
    w?.unmount()
    w = null
  })

  it('personal serve: no status entry, no poll; settings stays', async () => {
    // /api/status does not exist on a personal deployment — the SPA
    // fallback answers with HTML and the probe never parses.
    api.getStatus.mockRejectedValue(new Error('spa fallback'))

    mountSidebar()
    await flushPromises()

    expect(footerLabels()).toEqual(['Settings'])
    expect(w?.find('.status-lamp').exists()).toBe(false)
    // Mount probe only — the 20s poll never arms without the surface.
    expect(api.getStatus).toHaveBeenCalledTimes(1)
  })

  it('bridge deployment: status entry appears with the lamp', async () => {
    api.getStatus.mockResolvedValue(bridgeStatus())

    mountSidebar()
    await flushPromises()

    expect(footerLabels()).toEqual(['Status', 'Settings'])
    expect(w?.find('.status-lamp').exists()).toBe(true)
  })
})
