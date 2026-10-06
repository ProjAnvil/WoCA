// The composer's chip toolbar: popover toggling, mode/thinking/model
// picks (success and failure), the context popover (pinned + throttled
// refresh), quota, and the shared backdrop.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  getAgent: vi.fn(),
  getQuota: vi.fn(),
  setPermissionMode: vi.fn(),
  setThinkingLevel: vi.fn(),
  applyModel: vi.fn(),
  listModels: vi.fn(),
  getSettings: vi.fn(),
  getSessionContext: vi.fn(),
}))
vi.mock('../../api/client', () => ({ ...api }))

import i18n from '../../i18n'
import ComposerChips from './ComposerChips.vue'
import { useAgentStore } from '../../stores/agent'
import { useSessionStore } from '../../stores/sessions'
import { useServerSettingsStore } from '../../stores/serverSettings'
import { useToastStore } from '../../stores/toast'

function mountChips() {
  return mount(ComposerChips, { global: { plugins: [i18n, createPinia()] } })
}

// Click handlers await store actions; let the microtask chain finish
// before asserting DOM that the awaited tail mutates.
const flush = async (w: ReturnType<typeof mountChips>) => {
  await Promise.resolve()
  await Promise.resolve()
  await w.vm.$nextTick()
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  api.setPermissionMode.mockResolvedValue({ ok: true, mode: 'default' })
  api.setThinkingLevel.mockResolvedValue({ ok: true, level: 'off', budget: 0 })
  api.applyModel.mockResolvedValue({})
  api.getSessionContext.mockResolvedValue({})
  api.listModels.mockResolvedValue([])
  api.getSettings.mockResolvedValue({})
  api.getAgent.mockResolvedValue(undefined)
  api.getQuota.mockResolvedValue({})
  window.localStorage.clear()
})

describe('permission mode chip', () => {
  it('opens the popover and applies a picked mode', async () => {
    const w = mountChips()
    expect(w.find('.ctl-pop').exists()).toBe(false)
    await w.findAll('.ctl-chip')[0].trigger('click')
    expect(w.findAll('.ctl-opt').length).toBeGreaterThan(1)
    await w.findAll('.ctl-opt')[1].trigger('click')
    await flush(w)
    expect(api.setPermissionMode).toHaveBeenCalled()
    expect(w.find('.ctl-pop').exists()).toBe(false) // closed on success
  })

  it('keeps the popover open and toasts on a failed switch', async () => {
    api.setPermissionMode.mockRejectedValueOnce(new Error('locked'))
    const w = mountChips()
    await w.findAll('.ctl-chip')[0].trigger('click')
    await w.findAll('.ctl-opt')[1].trigger('click')
    await flush(w)
    const toast = useToastStore()
    expect(toast.toasts.length).toBeGreaterThan(0)
    expect(w.find('.ctl-pop').exists()).toBe(true)
  })
})

describe('model chip', () => {
  it('stays hidden without a catalog and picks persist the preference', async () => {
    const bare = mountChips()
    expect(bare.findAll('.ctl-chip').length).toBe(4) // mode/thinking/ctx/quota only

    const w = mountChips()
    await flush(w) // let onMounted's ensureModels settle before seeding the catalog
    const catalog = useServerSettingsStore()
    catalog.models = [
      { id: 'm1', name: 'One', model: 'model-1', provider: 'P' },
      { id: 'm2', name: 'Two', model: 'model-2', provider: 'P' },
    ] as never
    await w.vm.$nextTick()
    const modelChip = w.findAll('.ctl-chip')[1]
    await modelChip.trigger('click')
    await w.findAll('.ctl-opt')[1].trigger('click') // pick "Two"
    await flush(w)
    expect(api.applyModel).toHaveBeenCalledWith('m2')
    expect(window.localStorage.getItem('woca.pref.model')).toBe('m2')
    expect(w.find('.ctl-pop').exists()).toBe(false)
  })
})

describe('thinking chip', () => {
  it('is disabled when the model has no thinking support', async () => {
    const w = mountChips()
    const chip = w.findAll('.ctl-chip').at(-3)!
    expect(chip.attributes('disabled')).toBeDefined()
    await chip.trigger('click')
    expect(w.find('.ctl-pop').exists()).toBe(false)
  })

  it('picks a level when supported', async () => {
    const w = mountChips()
    await flush(w)
    const agent = useAgentStore()
    agent.state = {
      permission_mode: 'default',
      thinking: { supported: true, levels: ['low', 'high'], level: 'low' },
    } as never
    await w.vm.$nextTick()
    const chip = w.findAll('.ctl-chip').at(-3)!
    expect(chip.attributes('disabled')).toBeUndefined()
    await chip.trigger('click')
    const opts = w.findAll('.ctl-opt')
    await opts[opts.length - 1].trigger('click') // highest level
    await flush(w)
    expect(api.setThinkingLevel).toHaveBeenCalledWith('high')
    expect(w.find('.ctl-pop').exists()).toBe(false)
  })
})

describe('context chip', () => {
  it('is disabled on an empty transcript', async () => {
    const w = mountChips()
    const chip = w.findAll('.ctl-chip').at(-2)!
    expect(chip.attributes('disabled')).toBeDefined()
    await chip.trigger('click')
    expect(w.find('.ctx-pop').exists()).toBe(false)
  })

  it('pins open, refreshes throttled context, renders the window ring, and Esc closes', async () => {
    const w = mountChips()
    const store = useSessionStore()
    store.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
    )
    await w.vm.$nextTick()
    await w.findAll('.ctl-chip').at(-2)!.trigger('click')
    expect(w.find('.ctx-pop').exists()).toBe(true)
    expect(api.getSessionContext).toHaveBeenCalledTimes(1)

    store.contextInfo = {
      model: 'm',
      estimated_total: 5000,
      context_window: 100_000,
      compact_threshold: 80_000,
      last_request: { input_tokens: 2000, cache_read_tokens: 1000, cache_write_tokens: 500 },
      categories: [{ key: 'k', label: 'Tools', tokens: 1000, count: 2 }],
    } as never
    await w.vm.$nextTick()
    expect(w.get('.ctx-exact-num').text()).not.toBe('')
    expect(w.get('.ctx-ring-text').text()).toBe('5%')
    expect(w.get('.ctx-cat').text()).toContain('Tools')

    // A second click closes the pinned popover without refetching.
    await w.findAll('.ctl-chip').at(-2)!.trigger('click')
    expect(w.find('.ctx-pop').exists()).toBe(false)
    expect(api.getSessionContext).toHaveBeenCalledTimes(1)
  })

  it('renders the error channel instead of numbers when the probe fails', async () => {
    const w = mountChips()
    const store = useSessionStore()
    store.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
    )
    await w.vm.$nextTick() // ctxAvailable must propagate before the click
    await w.findAll('.ctl-chip').at(-2)!.trigger('click') // opens + fires the refresh
    await flush(w)
    store.contextError = 'core plane unreachable' // the probe failed after opening
    await w.vm.$nextTick()
    expect(w.get('.ctx-error').text()).toContain('core plane unreachable')
  })
})

describe('quota chip and the shared backdrop', () => {
  it('opens the quota popover and refreshes', async () => {
    const w = mountChips()
    await w.findAll('.ctl-chip').at(-1)!.trigger('click')
    expect(w.find('.quota-pop').exists()).toBe(true)
    expect(api.getQuota).toHaveBeenCalled()

    // The backdrop click closes whatever popover is open.
    expect(w.find('.ctl-backdrop').exists()).toBe(true)
    await w.get('.ctl-backdrop').trigger('click')
    expect(w.find('.quota-pop').exists()).toBe(false)
  })
})
