import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WebSearchSection from './WebSearchSection.vue'
import i18n from '../i18n'
import type { Settings } from '../types'

// The enterprise web-search section. What matters here is the managed-plane
// contract: an empty provider list means "not managed" (users choose), and a
// non-empty one pins the set on every machine the pack reaches.
function base(): Settings {
  return {
    search_providers: [],
    search_searxng_endpoint: '',
    search_custom_endpoint: '',
    search_custom_key_set: false,
    search_allowed_domains: [],
    search_denied_domains: [],
  } as Settings
}

function mountWith(over: Partial<Settings> = {}, probe: unknown = null) {
  const s: Settings = { ...base(), ...over }
  const drafts: Record<string, string> = {}
  const saves: Array<Record<string, unknown>> = []
  const calls: Array<{ method?: string; path?: string }> = []
  const api = async (path: string, init?: RequestInit) => {
    calls.push({ method: init?.method, path })
    return new Response(JSON.stringify(probe ?? {}), { status: 200 })
  }
  const w = mount(WebSearchSection, {
    global: {
      plugins: [i18n],
      provide: {
        cfg: {
          s,
          d: () => '',
          v: (f: string) => drafts[f] ?? '',
          setDraft: (f: string, v: string) => { drafts[f] = v },
          save: async (extra?: Record<string, unknown>) => { saves.push(extra ?? {}) },
          saving: false,
          savedFlash: '',
          error: '',
          api,
        },
      },
    },
  })
  return { w, saves, calls }
}

/** Toggles the backend card whose name contains `name`. */
async function toggle(w: ReturnType<typeof mountWith>['w'], name: string) {
  const row = w.findAll('.ws-row').find((r) => r.text().includes(name))
  if (!row) throw new Error(`no backend row for ${name}`)
  await row.find('.switch').trigger('click')
  await flushPromises()
}

describe('WebSearchSection (config plane)', () => {
  it('starts with nothing pinned so users keep their own choice', async () => {
    const { w, saves } = mountWith()
    await flushPromises()

    // No provider carries a position badge: nothing is pinned yet.
    expect(w.text()).not.toContain('#1')
    await w.findAll('button').find((b) => b.text() === 'Save')!.trigger('click')
    await flushPromises()
    expect(saves[0].search_providers).toEqual([])
  })

  it('pins the ordered set the operator picked', async () => {
    const { w, saves } = mountWith()

    // Enable SearXNG then DuckDuckGo — the order they are toggled is the
    // failover order. Rows are looked up by name each time: enabling one
    // re-sorts the list, so a cached index would hit the wrong backend.
    await toggle(w, 'SearXNG')
    await toggle(w, 'DuckDuckGo')

    expect(w.text()).toContain('#1')
    expect(w.text()).toContain('#2')

    await w.findAll('button').find((b) => b.text() === 'Save')!.trigger('click')
    await flushPromises()
    expect(saves[0].search_providers).toEqual(['searxng', 'ddg-html'])
  })

  it('reorders by dragging', async () => {
    const { w, saves } = mountWith({ search_providers: ['searxng', 'ddg-html'] })
    await flushPromises()

    const grips = w.findAll('.ws-grip')
    await grips[0].trigger('dragstart')
    await grips[1].trigger('dragenter')
    await grips[1].trigger('dragend')
    await flushPromises()

    await w.findAll('button').find((b) => b.text() === 'Save')!.trigger('click')
    await flushPromises()
    expect(saves[0].search_providers).toEqual(['ddg-html', 'searxng'])
  })

  // The probe is how an operator knows the chain they are about to ship
  // actually answers — a blocked backend looks like a search that found
  // nothing.
  it('runs the probe and reports the backend that answered', async () => {
    const { w, calls } = mountWith(
      {},
      { ok: true, provider: 'searxng', results: 3, latency_ms: 120, sample: 'Go — https://go.dev' },
    )
    await flushPromises()

    await w.findAll('button').find((b) => b.text().includes('test search'))!.trigger('click')
    await flushPromises()

    expect(calls.some((c) => c.path === '/api/websearch/test' && c.method === 'POST')).toBe(true)
    expect(w.text()).toContain('searxng')
    expect(w.text()).toContain('120')
  })

  it('shows the error when the probe fails', async () => {
    const { w } = mountWith({}, { ok: false, provider: '', results: 0, latency_ms: 40, error: 'rate limited' })
    await flushPromises()

    await w.findAll('button').find((b) => b.text().includes('test search'))!.trigger('click')
    await flushPromises()

    expect(w.find('.ws-test-bad').exists()).toBe(true)
    expect(w.text()).toContain('rate limited')
  })

  it('saves the comma-separated domain lists as arrays', async () => {
    const { w, saves } = mountWith()
    await flushPromises()

    const inputs = w.findAll('input')
    // Allowed list is the fourth input; denied is the fifth.
    await inputs[3].setValue('go.dev, pkg.go.dev')
    await inputs[4].setValue('spam.example')
    await w.findAll('button').find((b) => b.text() === 'Save')!.trigger('click')
    await flushPromises()

    expect(saves[0].search_allowed_domains).toEqual(['go.dev', 'pkg.go.dev'])
    expect(saves[0].search_denied_domains).toEqual(['spam.example'])
  })
})
