// Task 11: the session grants panel — lists the session's granted rule keys
// and revokes them one by one through the agent grants API.
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  listGrants: vi.fn(),
  revokeGrant: vi.fn(),
}))
vi.mock('../../api/client', () => ({ ...api }))

import i18n from '../../i18n'
import GrantsPanel from './GrantsPanel.vue'

function mountPanel(session = 'session-1') {
  return mount(GrantsPanel, {
    props: { open: true, session },
    global: { plugins: [i18n] },
  })
}

const flush = async (w: { vm: { $nextTick: () => Promise<void> } }) => {
  await Promise.resolve()
  await Promise.resolve()
  await w.vm.$nextTick()
}

beforeEach(() => {
  vi.clearAllMocks()
  api.listGrants.mockReset().mockResolvedValue({ grants: [] })
  api.revokeGrant.mockReset().mockResolvedValue(undefined)
})

describe('grants panel', () => {
  it('fetches the session grants when opened and renders one row per key', async () => {
    api.listGrants.mockResolvedValue({
      grants: [{ key: 'bash:git push*' }, { key: 'domain:example.com' }],
    })
    const w = mountPanel()
    await flush(w)
    expect(api.listGrants).toHaveBeenCalledWith('session-1')
    expect(w.find('[data-testid="grants-row-bash:git push*"]').exists()).toBe(true)
    expect(w.find('[data-testid="grants-row-domain:example.com"]').exists()).toBe(true)
  })

  it('revokes a grant and refreshes the list', async () => {
    api.listGrants.mockResolvedValueOnce({ grants: [{ key: 'bash:git push*' }] })
    const w = mountPanel()
    await flush(w)
    api.listGrants.mockResolvedValueOnce({ grants: [] })
    await w.get('[data-testid="grants-row-bash:git push*"] .grant-revoke').trigger('click')
    await flush(w)
    expect(api.revokeGrant).toHaveBeenCalledWith('bash:git push*', 'session-1')
    expect(api.listGrants).toHaveBeenCalledTimes(2)
    expect(w.find('[data-testid="grants-row-bash:git push*"]').exists()).toBe(false)
  })

  it('emits close from the backdrop', async () => {
    const w = mountPanel()
    await flush(w)
    await w.get('.hitl-grants-scrim').trigger('click')
    expect(w.emitted('close')).toBeTruthy()
  })

  it('closes from the lucide X icon button (labelled, no text glyph)', async () => {
    const w = mountPanel()
    await flush(w)
    const close = w.get('.card-head .icon-btn')
    expect(close.attributes('aria-label')).toBe(i18n.global.t('common.close'))
    expect(close.find('svg').exists()).toBe(true)
    expect(close.text()).not.toContain('✕')
    await close.trigger('click')
    expect(w.emitted('close')).toBeTruthy()
  })
})
