import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  listBridgeSessions: vi.fn(),
  listAudit: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import AdminSection from './AdminSection.vue'

function mountSection() {
  return mount(AdminSection, { global: { plugins: [i18n] } })
}

describe('AdminSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.listBridgeSessions.mockReset()
    api.listAudit.mockReset()
  })

  it('renders the live sessions and the audit trail side by side', async () => {
    api.listBridgeSessions.mockResolvedValue([
      { key_prefix: 'woca_1a', remark: 'alice', role: 'member', created_at: 1, last_seen: 2, expires_at: 3 },
    ])
    api.listAudit.mockResolvedValue([{ at: 4, actor: 'alice', action: 'login', detail: '' }])

    const w = mountSection()
    await flushPromises()

    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('member')
    expect(w.text()).toContain('login')
  })

  it('surfaces a failed inventory load', async () => {
    api.listBridgeSessions.mockRejectedValue(new Error('forbidden'))
    api.listAudit.mockResolvedValue([])
    const w = mountSection()
    await flushPromises()
    expect(w.text()).toContain('forbidden')
  })
})
