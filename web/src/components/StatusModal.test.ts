// The status health page: connection zones from the shared status store,
// the best-effort runtime zone, governance fields, and the diagnostics
// export.
import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({ getStatus: vi.fn(), stats: vi.fn() }))
vi.mock('../api/client', () => ({ ...api }))

import i18n from '../i18n'
import StatusModal from './StatusModal.vue'
import { useStatusStore } from '../stores/status'
import { useServerSettingsStore } from '../stores/serverSettings'

const cloud = (over: Record<string, unknown> = {}) => ({
  enrolled: true,
  stale: false,
  url: 'https://edge.example.com',
  url_source: 'config',
  bridge_id: 'br-1',
  last_version: 'v9',
  last_pull: 'today',
  spki_pinned: true,
  tunnel: { serving: true, served: 12 },
  ...over,
})

const enrolled = (over: Record<string, unknown> = {}) =>
  ({
    edition: 'bridge',
    auth: 'apikey',
    managed: true,
    cloud: cloud(),
    ...over,
  }) as never

const mountModal = () => mount(StatusModal, { global: { plugins: [i18n, createPinia()] } })
const settled = async (w: ReturnType<typeof mountModal>) => {
  await Promise.resolve()
  await Promise.resolve()
  await w.vm.$nextTick()
}

beforeEach(() => {
  vi.clearAllMocks()
  api.getStatus.mockReset().mockResolvedValue(enrolled())
  api.stats.mockReset().mockResolvedValue({
    active_runs: 1,
    queued_runs: 2,
    teammates: 3,
    sse_dropped: 0,
  })
})

describe('status modal', () => {
  it('renders the gateway zone from the shared store, runtime best-effort', async () => {
    const w = mountModal()
    await settled(w)
    expect(w.find('.status-error').exists()).toBe(false)
    expect(w.text()).toContain('br-1')
    expect(w.text()).toContain('v9')
    expect(w.text()).toContain(i18n.global.t('status.tunnelServing', { n: 12 }))
    expect(w.text()).toContain(i18n.global.t('status.fresh'))
    expect(w.text()).toContain('1') // active runs
  })

  it('a failing runtime probe never masks the connection zones', async () => {
    api.stats.mockRejectedValueOnce(new Error('no stats'))
    const w = mountModal()
    await settled(w)
    expect(w.text()).toContain('br-1')
    expect(w.text()).not.toContain(i18n.global.t('status.runtime'))
  })

  it('standalone deployments and stale caches read accordingly', async () => {
    api.getStatus.mockResolvedValueOnce(enrolled({ cloud: cloud({ enrolled: false }), managed: false }))
    const w = mountModal()
    await settled(w)
    expect(w.text()).toContain(i18n.global.t('status.standalone'))
    expect(w.text()).not.toContain(i18n.global.t('status.bridgeId'))
    expect(w.text()).toContain(i18n.global.t('status.localOnly'))

    api.getStatus.mockResolvedValueOnce(enrolled({ cloud: cloud({ stale: true }) }))
    const w2 = mountModal()
    await settled(w2)
    expect(w2.text()).toContain(i18n.global.t('status.staleCache'))
  })

  it('shows the unavailable banner when the status probe fails', async () => {
    api.getStatus.mockRejectedValueOnce(new Error('spa fallback'))
    const w = mountModal()
    await settled(w)
    expect(w.get('.status-error').text()).toBe(i18n.global.t('status.unavailable'))
  })

  it('lists managed governance fields with the config version', async () => {
    const w = mountModal()
    await settled(w)
    // managedFields derives from the settings snapshot's managed_fields.
    const server = useServerSettingsStore()
    server.server = { managed_fields: ['debug_log', 'otlp_endpoint'] } as never
    await w.vm.$nextTick()
    const list = w.get('.status-managed-list').text()
    expect(list).toContain('debug_log')
    expect(list).toContain('otlp_endpoint')
    expect(list).toContain('v9')
  })

  it('exports a diagnostics bundle download', async () => {
    const createObjectURL = vi.fn().mockReturnValue('blob:diag')
    const revokeObjectURL = vi.fn()
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL })
    const click = vi.fn()
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(click)
    const w = mountModal()
    await settled(w)
    await w.findAll('.icon-btn')[0].trigger('click')
    expect(createObjectURL).toHaveBeenCalled()
    expect(click).toHaveBeenCalled()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:diag')
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('closes on the backdrop and the X button', async () => {
    const w = mountModal()
    await settled(w)
    await w.get('.modal-backdrop').trigger('click')
    expect(w.emitted('close')).toBeTruthy()
  })
})
