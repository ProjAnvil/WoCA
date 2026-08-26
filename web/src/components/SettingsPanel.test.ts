import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../i18n'

const api = vi.hoisted(() => ({
  getStatus: vi.fn(),
  getEdgeBridges: vi.fn(),
  logout: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import SettingsPanel from './SettingsPanel.vue'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/settings'
import type { BridgeStatus } from '../api/client'
import type { AppSettings } from '../types'

function bridgeStatus(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    edition: 'bridge',
    auth: 'apikey',
    managed: true,
    cloud: {
      url: 'https://edge.example.com',
      url_source: 'env',
      token_source: 'env',
      token_set: true,
      enrolled: true,
      bridge_id: 'br_1',
      stale: false,
      spki_pinned: true,
      tunnel: { serving: true, served: 0 },
    },
    ...over,
  } as BridgeStatus
}

function mountPanel() {
  const pinia = createPinia()
  setActivePinia(pinia)
  return { pinia, w: mount(SettingsPanel, { global: { plugins: [i18n, pinia] } }) }
}

function navLabels(w: ReturnType<typeof mountPanel>['w']): string[] {
  return w.findAll('.settings-nav-item:not(.nav-logout)').map((b) => b.text())
}

describe('SettingsPanel', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset().mockResolvedValue({ edition: 'personal', managed: false })
    api.getEdgeBridges.mockReset().mockResolvedValue(null)
    api.logout.mockReset().mockResolvedValue(undefined)
  })

  it('keeps the personal shape: no bridge sections, no logout', async () => {
    const { w } = mountPanel()
    await flushPromises()
    const nav = navLabels(w)
    expect(nav.join('|')).not.toContain('Remote config')
    expect(nav.join('|')).not.toContain('Access point')
    expect(nav.join('|')).not.toContain('Administration')
    expect(w.find('.nav-logout').exists()).toBe(false)
  })

  it('shows the bridge surfaces for an enrolled admin', async () => {
    api.getStatus.mockResolvedValue(bridgeStatus())
    api.getEdgeBridges.mockResolvedValue([{ bridge_id: 'br_1', created_at: 1, last_seen: 2, live: true }])

    const { w } = mountPanel()
    const auth = useAuthStore()
    auth.me = { user_id: 'u1', remark: 'ops', role: 'admin' }
    await flushPromises()

    const nav = navLabels(w).join('|')
    expect(nav).toContain('Remote config')
    expect(nav).toContain('Access point')
    expect(nav).toContain('Cloud access')
    expect(nav).toContain('Admin')
    // Identity + logout pinned bottom-left.
    expect(w.find('.nav-identity').text()).toContain('ops')
    expect(w.find('.nav-logout').exists()).toBe(true)
  })

  it('members get the interactive surfaces but not governance', async () => {
    api.getStatus.mockResolvedValue(bridgeStatus())
    const { w } = mountPanel()
    const auth = useAuthStore()
    auth.me = { user_id: 'u2', remark: 'dev', role: 'member' }
    await flushPromises()

    const nav = navLabels(w).join('|')
    expect(nav).toContain('Remote config')
    expect(nav).not.toContain('Cloud access')
    expect(nav).not.toContain('Admin')
    // Logout exists for any session-holding deployment.
    expect(w.find('.nav-logout').exists()).toBe(true)
  })

  it('logout calls the API and reloads into the gate', async () => {
    const reload = vi.fn()
    vi.stubGlobal('location', {
      get href() {
        return 'http://localhost:3490/'
      },
      set href(_v: string) {
        /* navigation no-op */
      },
      reload,
    })
    api.getStatus.mockResolvedValue(bridgeStatus())
    const { w } = mountPanel()
    await flushPromises()

    await w.get('.nav-logout').trigger('click')
    await flushPromises()
    expect(api.logout).toHaveBeenCalledTimes(1)
    expect(reload).toHaveBeenCalledTimes(1)
    vi.unstubAllGlobals()
  })

  it('native mode shows the connect entry and disconnects via the binding', async () => {
    const reload = vi.fn()
    vi.stubGlobal('location', {
      get href() {
        return 'http://localhost:34115/'
      },
      set href(_v: string) {
        /* navigation no-op */
      },
      reload,
    })
    const logout = vi.fn().mockResolvedValue({ state: 'authorization_required' })
    ;(window as unknown as { go: unknown }).go = { main: { App: { Logout: logout } } }

    api.getStatus.mockResolvedValue(bridgeStatus())
    const { w } = mountPanel()
    await flushPromises()

    // The native-only entry appears alongside the bridge sections.
    const entry = w.findAll('.settings-nav-item').find((b) => b.text() === 'Connection')
    expect(entry).toBeTruthy()
    await entry!.trigger('click')

    await w.get('[data-testid="native-disconnect"]').trigger('click')
    await flushPromises()
    expect(logout).toHaveBeenCalledTimes(1)
    expect(reload).toHaveBeenCalledTimes(1)

    delete (window as unknown as { go: unknown }).go
    vi.unstubAllGlobals()
  })
})
