import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises } from '@vue/test-utils'
import i18n from '../i18n'

const api = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import { useServerSettingsStore } from './serverSettings'

function settingsDoc(over: Record<string, unknown> = {}) {
  return { provider: 'anthropic', msg_plane_available: false, ...over }
}

describe('serverSettings edition-scoped patch fields', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
  })

  it('omits message-plane and langfuse keys on forms without those surfaces', async () => {
    api.getSettings.mockResolvedValue(settingsDoc({ langfuse_console_managed: true }))
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    const s = useServerSettingsStore()
    await s.loadServer()
    await s.saveServer()
    await flushPromises()
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(patch.msg_plane_mode).toBeUndefined()
    expect(patch.langfuse_enabled).toBeUndefined()
    expect(patch.langfuse_base_url).toBeUndefined()
    expect(patch.langfuse_public_key).toBeUndefined()
  })

  it('sends langfuse keys on the personal form', async () => {
    api.getSettings.mockResolvedValue(settingsDoc({ langfuse_console_managed: false }))
    api.updateSettings.mockResolvedValue({ ok: true, restart_required: false })
    const s = useServerSettingsStore()
    await s.loadServer()
    s.langfuseEnabled = true
    s.langfuseBaseURL = 'https://lf.example'
    await s.saveServer()
    await flushPromises()
    const patch = api.updateSettings.mock.calls[0][0] as Record<string, unknown>
    expect(patch.langfuse_enabled).toBe(true)
    expect(patch.langfuse_base_url).toBe('https://lf.example')
  })
})
