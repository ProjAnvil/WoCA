// The web-search settings section: catalog render, enable/disable, drag
// reordering, save payload, and the verify probe.
//
// The verify path is the one that matters most — a throttled backend looks
// exactly like a search that found nothing, so the UI has to be able to show
// the difference.
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  getWebSearch: vi.fn(),
  putWebSearch: vi.fn(),
  testWebSearch: vi.fn(),
}))
vi.mock('../../api/settings', () => ({ ...api }))

import i18n from '../../i18n'
import WebSearchSection from './WebSearchSection.vue'
import type { WebSearchState } from '../../types'

const state = (over: Partial<WebSearchState> = {}): WebSearchState => ({
  providers: [
    {
      id: 'ddg-html',
      name: 'DuckDuckGo',
      description: 'No account needed.',
      needs_endpoint: false,
      enabled: true,
      order: 1,
    },
    {
      id: 'ddg-lite',
      name: 'DuckDuckGo (lightweight)',
      description: 'A different endpoint.',
      needs_endpoint: false,
      enabled: true,
      order: 2,
    },
    {
      id: 'searxng',
      name: 'SearXNG',
      description: 'Self-hosted.',
      needs_endpoint: true,
      enabled: false,
      order: 0,
    },
  ],
  enabled: ['ddg-html', 'ddg-lite'],
  searxng_endpoint: '',
  custom_endpoint: '',
  custom_key_set: false,
  allowed_domains: [],
  denied_domains: [],
  managed_fields: [],
  ...over,
})

const mountSection = () => mount(WebSearchSection, { global: { plugins: [i18n] } })

const settled = async () => {
  for (let i = 0; i < 4; i++) await Promise.resolve()
  await new Promise((r) => setTimeout(r, 0))
}

beforeEach(() => {
  vi.clearAllMocks()
  api.getWebSearch.mockResolvedValue(state())
  api.putWebSearch.mockResolvedValue({ ok: true })
  api.testWebSearch.mockResolvedValue({
    ok: true,
    provider: 'ddg-html',
    results: 5,
    latency_ms: 210,
    sample: 'Go — https://go.dev',
  })
})

describe('WebSearchSection', () => {
  it('renders the catalog with the effective failover order', async () => {
    const w = mountSection()
    await settled()

    const rows = w.findAll('.ws-row')
    expect(rows.length).toBe(3)
    // The two enabled backends are first and carry their position.
    expect(rows[0].text()).toContain('DuckDuckGo')
    expect(rows[0].text()).toContain('#1')
    expect(rows[1].text()).toContain('#2')
    // SearXNG is off, so it has no position badge.
    expect(rows[2].text()).toContain('SearXNG')
    expect(rows[2].text()).not.toContain('#')
  })

  it('shows the endpoint field only for a backend that needs one', async () => {
    const w = mountSection()
    await settled()
    expect(w.find('#ws-searxng').exists()).toBe(false)

    // Enabling SearXNG reveals its endpoint input.
    await w.findAll('.ws-row')[2].find('.switch').trigger('click')
    await settled()
    expect(w.find('#ws-searxng').exists()).toBe(true)
  })

  it('toggling a backend off removes it from the saved order', async () => {
    const w = mountSection()
    await settled()

    await w.findAll('.ws-row')[0].find('.switch').trigger('click')
    await settled()
    await w.find('.btn-primary').trigger('click')
    await settled()

    expect(api.putWebSearch).toHaveBeenCalledWith(
      expect.objectContaining({ providers: ['ddg-lite'] }),
    )
  })

  // Reordering is the failover order, so dragging must change what is saved.
  it('reorders by dragging and saves the new order', async () => {
    const w = mountSection()
    await settled()

    const grips = w.findAll('.ws-grip')
    await grips[0].trigger('dragstart')
    await grips[1].trigger('dragenter')
    await grips[1].trigger('dragend')
    await settled()

    await w.find('.btn-primary').trigger('click')
    await settled()

    expect(api.putWebSearch).toHaveBeenCalledWith(
      expect.objectContaining({ providers: ['ddg-lite', 'ddg-html'] }),
    )
  })

  it('parses the comma-separated domain lists before sending them', async () => {
    const w = mountSection()
    await settled()

    await w.find('#ws-allow').setValue('go.dev, pkg.go.dev')
    await w.find('#ws-deny').setValue('spam.example')
    await w.find('.btn-primary').trigger('click')
    await settled()

    expect(api.putWebSearch).toHaveBeenCalledWith(
      expect.objectContaining({
        allowed_domains: ['go.dev', 'pkg.go.dev'],
        denied_domains: ['spam.example'],
      }),
    )
  })

  it('does not send an empty custom key, so the stored one survives', async () => {
    const w = mountSection()
    await settled()
    await w.find('.btn-primary').trigger('click')
    await settled()

    const body = api.putWebSearch.mock.calls[0][0] as Record<string, unknown>
    expect(body.custom_key).toBeUndefined()
  })

  it('reports a successful test search with the backend that served it', async () => {
    const w = mountSection()
    await settled()

    await w.find('.btn').trigger('click')
    await settled()

    const out = w.find('.ws-test')
    expect(out.exists()).toBe(true)
    expect(out.text()).toContain('ddg-html')
    expect(out.text()).toContain('5')
    expect(out.text()).toContain('210')
    expect(w.find('.ws-test-bad').exists()).toBe(false)
  })

  // A throttled backend and a search that found nothing look identical to the
  // model; the UI has to tell the user which one happened.
  it('shows the error and the blocked-network hint when the test fails', async () => {
    api.testWebSearch.mockResolvedValue({
      ok: false,
      provider: '',
      results: 0,
      latency_ms: 90,
      error: 'rate limited',
    })
    const w = mountSection()
    await settled()

    await w.find('.btn').trigger('click')
    await settled()

    const out = w.find('.ws-test-bad')
    expect(out.exists()).toBe(true)
    expect(out.text()).toContain('rate limited')
    expect(out.text()).toContain('SearXNG')
  })

  it('does not let a locked provider list be edited', async () => {
    api.getWebSearch.mockResolvedValue(state({ managed_fields: ['search-providers'] }))
    const w = mountSection()
    await settled()

    const sw = w.findAll('.ws-row')[0].find('.switch')
    expect(sw.attributes('disabled')).toBeDefined()
    await sw.trigger('click')
    await settled()
    await w.find('.btn-primary').trigger('click')
    await settled()

    // The order the UI loaded is what gets sent back, unchanged.
    expect(api.putWebSearch).toHaveBeenCalledWith(
      expect.objectContaining({ providers: ['ddg-html', 'ddg-lite'] }),
    )
  })

  it('surfaces a load failure instead of rendering an empty section', async () => {
    api.getWebSearch.mockRejectedValue(new Error('boom'))
    const w = mountSection()
    await settled()

    expect(w.find('.row-hint').text()).toContain('boom')
  })
})
