import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'

import EdgeSwitchSection from './EdgeSwitchSection.vue'
import type { EdgeBridge } from '../../api/client'

const bridges: EdgeBridge[] = [
  { bridge_id: 'alpha', created_at: 1, last_seen: 50, live: true },
  { bridge_id: 'beta', created_at: 2, last_seen: 1, live: false },
]

function mountSection(current?: string) {
  return mount(EdgeSwitchSection, {
    props: { bridges, current },
    global: { plugins: [i18n] },
  })
}

describe('EdgeSwitchSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })

  it('renders the inventory with live state and the current marker', () => {
    const w = mountSection('alpha')

    const alpha = w.get('[data-testid="edge-bridge-alpha"]')
    expect(alpha.text()).toContain('alpha')
    expect(alpha.text()).toContain('current')
    expect(alpha.get('[data-testid="edge-dot"]').classes()).toContain('live')

    const beta = w.get('[data-testid="edge-bridge-beta"]')
    expect(beta.text()).toContain('offline')
    expect(beta.text()).not.toContain('current')
  })

  it('switches by navigating to /edge/select with the encoded bridge id', () => {
    const hrefs: string[] = []
    vi.stubGlobal('location', {
      get href() {
        return 'https://edge.example.com/'
      },
      set href(v: string) {
        hrefs.push(v)
      },
    })

    const w = mountSection('alpha')
    void w.get('[data-testid="edge-bridge-beta"]').trigger('click')

    expect(hrefs).toEqual(['/edge/select?bridge=beta'])
    vi.unstubAllGlobals()
  })

  it('emits refresh for the inventory re-probe', async () => {
    const w = mountSection('alpha')
    await w.get('.edge-refresh').trigger('click')
    expect(w.emitted('refresh')).toHaveLength(1)
  })

  it('adds a bridge by id through the same selection route', async () => {
    const hrefs: string[] = []
    vi.stubGlobal('location', {
      get href() {
        return 'https://edge.example.com/'
      },
      set href(v: string) {
        hrefs.push(v)
      },
    })
    const w = mountSection()
    await w.get('input.field').setValue('br_secret2')
    await w.get('.edge-add .btn').trigger('click')
    expect(hrefs).toEqual(['/edge/select?bridge=br_secret2'])
    vi.unstubAllGlobals()
  })

  it('navigates cross-origin for a URL target and rejects garbage', async () => {
    const hrefs: string[] = []
    vi.stubGlobal('location', {
      get href() {
        return 'https://edge.example.com/'
      },
      set href(v: string) {
        hrefs.push(v)
      },
    })
    const w = mountSection()
    const input = w.get('input.field')
    const go = w.get('.edge-add .btn')

    await input.setValue('https://other-edge.example.com/x')
    await go.trigger('click')
    expect(hrefs).toEqual(['https://other-edge.example.com'])

    await input.setValue('not a bridge!')
    await go.trigger('click')
    expect(w.text()).toContain('Unrecognized')
    expect(hrefs).toHaveLength(1)
    vi.unstubAllGlobals()
  })
})
