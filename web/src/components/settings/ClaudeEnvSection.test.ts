import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getClaudeEnv: vi.fn(),
  putClaudeEnv: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import ClaudeEnvSection from './ClaudeEnvSection.vue'

function envFixture() {
  return {
    skills: [{ name: 'bridge-deploy', description: 'fixture on the bridge machine' }],
    mcp: [{ name: 'bridge-echo', type: 'stdio', command: 'echo bridge-mcp' }],
    selected: { skills: [], mcp: [] },
  }
}

function mountSection(bridge?: boolean) {
  return mount(ClaudeEnvSection, {
    props: { bridge },
    global: { plugins: [i18n, createPinia()] },
  })
}

describe('ClaudeEnvSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
    api.getClaudeEnv.mockReset()
    api.putClaudeEnv.mockReset()
  })

  it('lists detected skills and MCP with empty states', async () => {
    api.getClaudeEnv.mockResolvedValue(envFixture())
    const w = mountSection()
    await flushPromises()

    expect(w.text()).toContain('bridge-deploy')
    expect(w.text()).toContain('stdio · echo bridge-mcp')
  })

  it('explains the bridge-machine semantics in the bridge form', async () => {
    api.getClaudeEnv.mockResolvedValue(envFixture())
    const w = mountSection(true)
    await flushPromises()
    expect(w.find('.mem-intro').text()).toContain('BRIDGE machine')
  })

  it('ticking a skill and saving PUTs the selection', async () => {
    api.getClaudeEnv.mockResolvedValue(envFixture())
    api.putClaudeEnv.mockResolvedValue({})
    const w = mountSection(true)
    await flushPromises()

    await w.get('.env-check input[type="checkbox"]').setValue()
    await w.get('.setting-row .btn').trigger('click')
    await flushPromises()
    expect(api.putClaudeEnv).toHaveBeenCalledWith({ skills: ['bridge-deploy'], mcp: [] })
  })

  it('surfaces a failed detection instead of an empty list', async () => {
    api.getClaudeEnv.mockRejectedValue(new Error('读取失败：forbidden'))
    const w = mountSection()
    await flushPromises()
    expect(w.text()).toContain('读取失败：forbidden')
  })
})
