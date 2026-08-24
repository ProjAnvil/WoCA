import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia } from 'pinia'
import i18n from '../../i18n'

const getManagedDelivery = vi.fn()
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, getManagedDelivery: (...a: Parameters<typeof getManagedDelivery>) => getManagedDelivery(...a) }
})

import ManagedSection from './ManagedSection.vue'
import type { BridgeStatus } from '../../api/client'

function enrolledStatus(over: Partial<BridgeStatus['cloud']> = {}): BridgeStatus {
  return {
    edition: 'bridge',
    managed: true,
    cloud: {
      url: 'https://edge.example.com',
      url_source: 'settings',
      token_source: 'settings',
      token_set: true,
      enrolled: true,
      bridge_id: 'br-1',
      last_version: '42',
      last_pull: '2026-08-24T01:02:03Z',
      stale: false,
      spki_pinned: true,
      tunnel: { serving: true, served: 3 },
      ...over,
    },
  }
}

function mountSection(status?: BridgeStatus | null) {
  return mount(ManagedSection, {
    props: { status: status ?? null },
    global: { plugins: [i18n, createPinia()] },
  })
}

describe('ManagedSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    getManagedDelivery.mockReset()
  })

  it('renders the delivered skills and MCP with the sync line', async () => {
    getManagedDelivery.mockResolvedValue({
      skills: [{ name: 'deploy', description: 'ship the stack' }],
      mcp: [{ name: 'search', type: 'http', url: 'https://search.internal/mcp' }],
    })

    const w = mountSection(enrolledStatus())
    await flushPromises()

    expect(w.get('[data-testid="managed-skill-deploy"]').text()).toContain('ship the stack')
    expect(w.get('[data-testid="managed-mcp-search"]').text()).toContain('https://search.internal/mcp')
    const sync = w.get('[data-testid="managed-sync"]').text()
    expect(sync).toContain('42')
  })

  it('shows empty states when nothing was delivered', async () => {
    getManagedDelivery.mockResolvedValue({ skills: [], mcp: [] })

    const w = mountSection(enrolledStatus())
    await flushPromises()

    expect(w.text()).toContain('No skills delivered by the config plane')
    expect(w.text()).toContain('No MCP servers delivered by the config plane')
  })

  it('flags the STALE cache posture from the cloud snapshot', async () => {
    getManagedDelivery.mockResolvedValue({ skills: [], mcp: [] })

    const w = mountSection(enrolledStatus({ stale: true }))
    await flushPromises()

    expect(w.get('[data-testid="managed-sync"]').text()).toContain('Cached')
  })

  it('surfaces a failed fetch instead of an empty success', async () => {
    getManagedDelivery.mockRejectedValue(new Error('boom'))

    const w = mountSection(enrolledStatus())
    await flushPromises()

    expect(w.text()).toContain('boom')
  })
})
