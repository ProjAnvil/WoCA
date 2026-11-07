// The read-only todo list: all four statuses render (each with its sr-only
// status text — never color alone), the progress summary counts, and the
// empty state explains itself.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import TodoTab from './TodoTab.vue'
import { useActivityStore } from '../../stores/activity'
import type { ActivitySnapshot, TodoItem } from '../../types'

const api = vi.hoisted(() => ({
  getActivity: vi.fn(),
  getShellOutput: vi.fn(),
  getTaskOutput: vi.fn(),
  killShell: vi.fn(),
  stopTask: vi.fn(),
  deleteCron: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  ...api,
}))

const todo = (content: string, status: TodoItem['status'], active_form?: string): TodoItem => ({
  content,
  status,
  ...(active_form ? { active_form } : {}),
})

describe('TodoTab', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  async function mountWith(items: TodoItem[]) {
    const activity = useActivityStore()
    api.getActivity.mockResolvedValueOnce({
      session_id: 's1',
      server_time: '2026-11-07T00:00:00Z',
      todos: { items, updated_at: '2026-11-07T00:00:00Z' },
      shells: [],
      tasks: [],
      crons: [],
      wake: { enabled: true, capped: false, stalled: false, pending_notifications: 0 },
    } satisfies ActivitySnapshot)
    await activity.fetchNow('s1')
    return mount(TodoTab, { props: { sid: 's1' }, global: { plugins: [i18n] } })
  }

  it('renders all four statuses with readable status text', async () => {
    const w = await mountWith([
      todo('pending item', 'pending'),
      todo('active item', 'in_progress', 'working on it'),
      todo('done item', 'completed'),
      todo('dropped item', 'cancelled'),
    ])
    const rows = w.findAll('.act-todo-row')
    expect(rows).toHaveLength(4)
    const text = w.text()
    expect(text).toContain('pending item')
    expect(text).toContain('done item')
    expect(text).toContain('dropped item')
    // in_progress leads with the present-tense label, falls back to content
    expect(text).toContain('working on it')
    // sr-only status words are present for each status
    expect(text).toContain(i18n.global.t('activity.todoStatus.pending'))
    expect(text).toContain(i18n.global.t('activity.todoStatus.in_progress'))
    expect(text).toContain(i18n.global.t('activity.todoStatus.completed'))
    expect(text).toContain(i18n.global.t('activity.todoStatus.cancelled'))
  })

  it('shows the progress summary', async () => {
    const w = await mountWith([todo('a', 'completed'), todo('b', 'completed'), todo('c', 'pending')])
    expect(w.get('.act-todo-progress').text()).toBe(
      i18n.global.t('activity.todoProgress', { done: 2, total: 3 }),
    )
  })

  it('renders the empty state when no todos exist', async () => {
    const w = await mountWith([])
    expect(w.get('.act-empty').text()).toContain(i18n.global.t('activity.todoEmpty'))
  })
})
