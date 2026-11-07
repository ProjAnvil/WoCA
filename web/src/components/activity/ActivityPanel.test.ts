// The popover card: dialog semantics, tablist with counts (click + arrow
// roving), and the four explicit data states — loading (never an empty
// list), error with retry, unavailable (501), ready with per-tab empty
// states — plus the stale strip and the close event.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../api/transport'
import i18n from '../../i18n'
import ActivityPanel from './ActivityPanel.vue'
import { useActivityStore } from '../../stores/activity'
import type {
  ActivitySnapshot,
  BackgroundShell,
  CronEntry,
  TodoItem,
} from '../../types'

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

const flush = async () => {
  for (let i = 0; i < 10; i++) await Promise.resolve()
}

const snap = (sid: string, over: Partial<ActivitySnapshot> = {}): ActivitySnapshot => ({
  session_id: sid,
  server_time: '2026-11-07T00:00:00Z',
  todos: { items: [], updated_at: null },
  shells: [],
  tasks: [],
  crons: [],
  wake: { enabled: true, capped: false, stalled: false, pending_notifications: 0 },
  ...over,
})

const shell = (id: string, status: BackgroundShell['status']): BackgroundShell => ({
  id,
  command: `cmd-${id}`,
  status,
  exit_code: status === 'running' ? null : 0,
  stopped_by: '',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:01Z',
  output_bytes: 0,
})

const cron = (id: string): CronEntry => ({
  id,
  name: `cron-${id}`,
  spec: '*/15 * * * *',
  prompt: 'check',
  created_at: '2026-11-07T00:00:00Z',
  last_fired_at: null,
  next_fire_at: '2026-11-07T00:15:00Z',
})

const todo = (content: string, status: TodoItem['status']): TodoItem => ({ content, status })

let activity: ReturnType<typeof useActivityStore>

function mountPanel(sid = 's1', attach = false) {
  return mount(ActivityPanel, {
    props: { sid },
    global: { plugins: [i18n] },
    ...(attach ? { attachTo: document.body } : {}),
  })
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  activity = useActivityStore()
})

describe('ActivityPanel dialog + tabs', () => {
  it('renders as a non-modal dialog with a tablist and three tabs', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountPanel()
    const dialog = w.get('[role="dialog"]')
    expect(dialog.attributes('aria-modal')).toBe('false')
    expect(dialog.attributes('aria-label')).toBe(i18n.global.t('activity.title'))
    expect(w.findAll('[role="tab"]').map((x) => x.text())).toEqual([
      expect.stringContaining(i18n.global.t('activity.tabTodo')),
      expect.stringContaining(i18n.global.t('activity.tabBackground')),
      expect.stringContaining(i18n.global.t('activity.tabScheduled')),
    ])
    // background is the default tab
    expect(w.get('[role="tab"][data-tab="background"]').attributes('aria-selected')).toBe('true')
  })

  it('shows each tab count from the snapshot (todo done/total, bg total, crons)', async () => {
    api.getActivity.mockResolvedValueOnce(
      snap('s1', {
        todos: {
          items: [todo('a', 'completed'), todo('b', 'completed'), todo('c', 'pending')],
          updated_at: '2026-11-07T00:00:00Z',
        },
        shells: [shell('s1', 'running'), shell('s2', 'completed')],
        crons: [cron('c1')],
      }),
    )
    await activity.fetchNow('s1')
    const w = mountPanel()
    expect(w.get('[data-tab="todo"] .act-tab-count').text()).toBe('2/3')
    expect(w.get('[data-tab="background"] .act-tab-count').text()).toBe('2')
    expect(w.get('[data-tab="scheduled"] .act-tab-count').text()).toBe('1')
  })

  it('switches tabs on click and on ArrowRight/ArrowLeft with roving focus', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountPanel('s1', true)
    try {
      const tabs = () => w.findAll('[role="tab"]')
      expect(tabs()[1].attributes('tabindex')).toBe('0')
      expect(tabs()[2].attributes('tabindex')).toBe('-1')

      await tabs()[2].trigger('click')
      expect(w.get('[data-tab="scheduled"]').attributes('aria-selected')).toBe('true')

      await w.get('.act-tabs').trigger('keydown', { key: 'ArrowRight' })
      await flush()
      // wraps around to the first tab
      expect(w.get('[data-tab="todo"]').attributes('aria-selected')).toBe('true')
      expect(document.activeElement?.getAttribute('data-tab')).toBe('todo')

      await w.get('.act-tabs').trigger('keydown', { key: 'ArrowLeft' })
      await flush()
      expect(w.get('[data-tab="scheduled"]').attributes('aria-selected')).toBe('true')
    } finally {
      w.unmount()
      document.body.innerHTML = ''
    }
  })

  it('emits close from the header close button', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountPanel()
    await w.get('.act-close').trigger('click')
    expect(w.emitted('close')).toHaveLength(1)
  })
})

describe('ActivityPanel data states', () => {
  it('renders a loading skeleton for an unseen session (never an empty list)', () => {
    const w = mountPanel('never-seen')
    expect(w.get('.act-loading').attributes('aria-busy')).toBe('true')
    expect(w.find('.act-empty').exists()).toBe(false)
  })

  it('demo mode: mount seeds an empty snapshot so tabs render, not a skeleton', async () => {
    activity.setDemo(true)
    const w = mountPanel('s1')
    await flush()
    expect(activity.statusOf('s1')).toBe('ready')
    expect(w.find('.act-loading').exists()).toBe(false)
    expect(w.get('[aria-labelledby="act-tab-background"]').text()).toContain(
      i18n.global.t('activity.backgroundEmpty'),
    )
    expect(api.getActivity).not.toHaveBeenCalled()
  })

  it('error state shows the reason and retries via fetchNow', async () => {
    api.getActivity.mockRejectedValueOnce(new Error('net down'))
    await activity.fetchNow('s1')
    const w = mountPanel()
    expect(w.get('.act-error').text()).toContain('net down')
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await w.get('.act-error button').trigger('click')
    await flush()
    expect(api.getActivity).toHaveBeenCalledTimes(2)
    expect(activity.statusOf('s1')).toBe('ready')
  })

  it('unavailable (S-B-0501) renders the deployment note, not an error or list', async () => {
    api.getActivity.mockRejectedValueOnce(new ApiError('nope', 'S-B-0501'))
    await activity.fetchNow('s1')
    const w = mountPanel()
    expect(w.get('.act-body .act-empty').text()).toContain(i18n.global.t('activity.unavailable'))
    expect(w.find('.act-error').exists()).toBe(false)
  })

  it('shows a stale strip when a signal arrived while the session was inactive', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    activity.onActivityChanged({ kind: 'todos', session_id: 's1' }, 'other')
    expect(activity.stateOf('s1').stale).toBe(true)
    const w = mountPanel()
    expect(w.get('.act-stale').text()).toContain(i18n.global.t('activity.staleNotice'))
  })

  it('passes the capped strip "send a message" action through as compose', async () => {
    api.getActivity.mockResolvedValueOnce(
      snap('s1', {
        crons: [cron('c1')],
        wake: { enabled: true, capped: true, stalled: false, pending_notifications: 1 },
      }),
    )
    await activity.fetchNow('s1')
    const w = mountPanel()
    await w.get('[data-tab="scheduled"]').trigger('click')
    await w.get('.act-wake-action').trigger('click')
    expect(w.emitted('compose')).toHaveLength(1)
  })

  it('per-tab empty states under a ready snapshot', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountPanel()
    // background is the default tab
    expect(w.get('[aria-labelledby="act-tab-background"]').text()).toContain(
      i18n.global.t('activity.backgroundEmpty'),
    )
    await w.get('[data-tab="todo"]').trigger('click')
    expect(w.get('[aria-labelledby="act-tab-todo"]').text()).toContain(
      i18n.global.t('activity.todoEmpty'),
    )
    await w.get('[data-tab="scheduled"]').trigger('click')
    expect(w.get('[aria-labelledby="act-tab-scheduled"]').text()).toContain(
      i18n.global.t('activity.scheduledEmpty'),
    )
  })
})
