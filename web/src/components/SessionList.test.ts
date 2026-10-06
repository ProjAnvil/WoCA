// The workspace-grouped session list: group drawers, row status icons,
// relative timestamps, and the confirm-gated delete flow.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import SessionList from './SessionList.vue'
import { useSessionStore } from '../stores/sessions'
import { useConfirmStore } from '../stores/confirm'
import { useToastStore } from '../stores/toast'

const iso = (minutesAgo: number) => new Date(Date.now() - minutesAgo * 60_000).toISOString()

interface SeedSession {
  id: string
  title?: string
  updatedAt?: string
  workspace?: string
  workspaceName?: string
}

// Mount first, THEN seed: mounting installs its own pinia, and the store
// handle the test mutates must be the component's instance.
async function mountSeeded(sessions: SeedSession[]) {
  const w = mount(SessionList, { global: { plugins: [i18n, createPinia()] } })
  const store = useSessionStore()
  store.seed(
    sessions.map((s) => ({
      title: 'T',
      updatedAt: '',
      loaded: true,
      workspace: '',
      workspaceName: '',
      ...s,
    })) as never,
    {},
  )
  await w.vm.$nextTick()
  return { w, store }
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.restoreAllMocks()
})

describe('workspace groups', () => {
  it('groups sessions by workspace and shows the count', async () => {
    const { w } = await mountSeeded([
      { id: 'a', workspace: '/w1', workspaceName: 'one' },
      { id: 'b', workspace: '/w1', workspaceName: 'one' },
      { id: 'c', workspace: '/w2', workspaceName: 'two' },
    ])
    expect(w.findAll('.ws-group-name').map((n) => n.text())).toEqual(['one', 'two'])
    expect(w.findAll('.ws-group-count').map((c) => c.text())).toEqual(['2', '1'])
    expect(w.findAll('.session-row')).toHaveLength(3)
  })

  it('collapses a group and re-expands it when it becomes active', async () => {
    const { w, store } = await mountSeeded([
      { id: 'a', workspace: '/w1', workspaceName: 'one' },
      { id: 'c', workspace: '/w2', workspaceName: 'two' },
    ])
    store.activeId = 'c' // make /w2 the active workspace…
    await w.vm.$nextTick()
    await w.findAll('.ws-group-toggle')[0].trigger('click') // …then collapse /w1
    expect(w.findAll('.ws-sessions')).toHaveLength(1) // w2 still open
    expect(w.get('.ws-group-toggle').attributes('aria-expanded')).toBe('false')

    // Switching onto a session in the collapsed group re-opens it.
    store.activeId = 'a'
    await w.vm.$nextTick()
    expect(w.findAll('.ws-sessions')).toHaveLength(2)
  })
})

describe('session rows', () => {
  it('marks the active row and swaps the icon for running/awaiting states', async () => {
    const { w, store } = await mountSeeded([{ id: 'a' }, { id: 'b' }, { id: 'c' }])
    store.activeId = 'a'
    store.status = { b: 'running', c: 'awaiting' }
    await w.vm.$nextTick()
    const rows = w.findAll('.session-row')
    expect(rows[0].classes()).toContain('active')
    expect(rows[1].find('.spinning').exists()).toBe(true)
    expect(rows[2].find('.awaiting').exists()).toBe(true)
    expect(rows[0].find('.spinning').exists()).toBe(false)
  })

  it('renders relative timestamps across the buckets', async () => {
    const { w } = await mountSeeded([
      { id: 'a', updatedAt: iso(0.2) },
      { id: 'b', updatedAt: iso(5) },
      { id: 'c', updatedAt: iso(3 * 60) },
      { id: 'd', updatedAt: iso(3 * 24 * 60) },
      { id: 'e', updatedAt: 'not-a-date' },
    ])
    const times = w.findAll('.session-time').map((el) => el.text())
    expect(times[0]).toBe(i18n.global.t('sidebar.justNow'))
    expect(times[1]).toBe(i18n.global.t('sidebar.minutesAgo', { min: 5 }))
    expect(times[2]).toBe(i18n.global.t('sidebar.hoursAgo', { hr: 3 }))
    expect(times[3]).toBe(i18n.global.t('sidebar.daysAgo', { days: 3 }))
    expect(times[4]).toBe('') // unparseable stamps stay empty, never NaN text
  })
})

describe('delete flow', () => {
  it('asks for confirmation and deletes only on confirm', async () => {
    const { w, store } = await mountSeeded([{ id: 'a', title: 'Named' }])
    const confirmDialog = useConfirmStore()
    const removeSpy = vi.spyOn(store, 'removeSession').mockResolvedValue()
    vi.spyOn(confirmDialog, 'ask').mockResolvedValue(false)
    await w.get('.session-del').trigger('click')
    expect(confirmDialog.ask).toHaveBeenCalled()
    expect(removeSpy).not.toHaveBeenCalled()

    vi.spyOn(confirmDialog, 'ask').mockResolvedValue(true)
    await w.get('.session-del').trigger('click')
    expect(removeSpy).toHaveBeenCalledWith('a')
  })

  it('toasts when the server refuses (e.g. a live run)', async () => {
    const { w, store } = await mountSeeded([{ id: 'a', title: 'Named' }])
    const confirmDialog = useConfirmStore()
    vi.spyOn(confirmDialog, 'ask').mockResolvedValue(true)
    vi.spyOn(store, 'removeSession').mockRejectedValue(new Error('run in flight'))
    await w.get('.session-del').trigger('click')
    await new Promise((r) => setTimeout(r, 0))
    const toast = useToastStore()
    expect(toast.toasts.some((x) => x.message.includes('run in flight'))).toBe(true)
  })
})
