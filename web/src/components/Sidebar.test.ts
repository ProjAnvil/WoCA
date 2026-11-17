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
import { useUiStore } from '../stores/ui'
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
  // attachTo: focus-trap assertions need the drawer in the document —
  // happy-dom only tracks document.activeElement for attached trees.
  w = mount(Sidebar, { global: { plugins: [i18n, pinia] }, attachTo: document.body })
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

describe('Sidebar drawer (≤640px shells)', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset()
  })
  afterEach(() => {
    w?.unmount()
    w = null
  })

  it('is collapsed by default: no open modifier, no backdrop', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    expect(w?.get('#app-sidebar.sidebar').classes()).not.toContain('open')
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('opens with the open modifier + backdrop; backdrop click closes', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await w?.vm.$nextTick()

    expect(w?.get('#app-sidebar.sidebar').classes()).toContain('open')
    const backdrop = w?.get('.sidebar-backdrop')
    expect(backdrop).toBeTruthy()
    await backdrop!.trigger('click')
    expect(ui.sidebarOpen).toBe(false)
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('closes on Escape while open', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()

    expect(ui.sidebarOpen).toBe(false)
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('moves focus inside the drawer when it opens', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()

    const aside = document.getElementById('app-sidebar')!
    expect(aside.contains(document.activeElement)).toBe(true)
  })

  it('keeps the drawer open when Escape belongs to a hosted overlay', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()
    // The workspaces "+" opens the new-workspace dialog (a hand-rolled
    // .overlay without its own trap) and focuses its input: Esc there
    // must close the dialog, not the drawer underneath.
    await w?.findAll('.side-section-head .icon-btn')[0]!.trigger('click')
    await flushPromises()
    expect(w?.find('.overlay .ws-dialog').exists()).toBe(true)
    expect(document.activeElement?.id).toBe('ws-path-input')

    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()

    expect(w?.find('.overlay .ws-dialog').exists()).toBe(false)
    expect(ui.sidebarOpen).toBe(true)
  })
})
