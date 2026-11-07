// The header entry: collapsed by default, anchored non-modal popover with
// open/close behaviors (click / Esc with focus return / outside pointer),
// badge priority (unviewed failures → danger, else running count → brand),
// accessible label carrying the counts, and the open-time store hooks
// (refresh-on-open for demo seeding, markViewed once Background shows).
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import ActivityTrigger from './ActivityTrigger.vue'
import { useActivityStore } from '../../stores/activity'
import { useSessionStore } from '../../stores/sessions'
import type { ActivitySnapshot, BackgroundShell } from '../../types'

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

const shell = (id: string, status: BackgroundShell['status']): BackgroundShell => ({
  id,
  command: `cmd-${id}`,
  status,
  exit_code: status === 'failed' ? 1 : status === 'running' ? null : 0,
  stopped_by: '',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:01Z',
  output_bytes: 0,
})

const snap = (sid: string, over: Partial<ActivitySnapshot> = {}): ActivitySnapshot => ({
  session_id: sid,
  server_time: new Date().toISOString(),
  todos: { items: [], updated_at: null },
  shells: [],
  tasks: [],
  crons: [],
  wake: { enabled: true, capped: false, stalled: false, pending_notifications: 0 },
  ...over,
})

const SESSION = { id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }

let activity: ReturnType<typeof useActivityStore>

function mountAttached() {
  return mount(ActivityTrigger, {
    global: { plugins: [i18n] },
    attachTo: document.body,
  })
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  activity = useActivityStore()
  useSessionStore().seed([SESSION] as never, {})
})

afterEach(() => {
  document.body.innerHTML = ''
})

const triggerButton = () => document.querySelector('.act-trigger button') as HTMLButtonElement
const popover = () => document.querySelector('.act-pop')

describe('ActivityTrigger collapsed state', () => {
  it('renders collapsed by default with dialog aria wiring on the button', () => {
    const w = mountAttached()
    const btn = w.get('button')
    expect(popover()).toBeNull()
    expect(btn.attributes('aria-expanded')).toBe('false')
    expect(btn.attributes('aria-haspopup')).toBe('dialog')
    expect(btn.attributes('aria-controls')).toBeTruthy()
    w.unmount()
  })
})

describe('ActivityTrigger open/close', () => {
  it('opens on click: teleported dialog appears, aria-expanded syncs', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    const pop = popover()
    expect(pop).toBeTruthy()
    expect(pop!.querySelector('[role="dialog"]')).toBeTruthy()
    expect(triggerButton().getAttribute('aria-expanded')).toBe('true')
    // aria-controls points at the rendered dialog
    expect(pop!.querySelector(`#${triggerButton().getAttribute('aria-controls')}`)).toBeTruthy()
    w.unmount()
  })

  it('closes on Esc and returns focus to the trigger button', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    popover()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await flush()
    expect(popover()).toBeNull()
    expect(document.activeElement).toBe(triggerButton())
    expect(triggerButton().getAttribute('aria-expanded')).toBe('false')
    w.unmount()
  })

  it('closes on an outside pointerdown (without stealing focus)', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    document.body.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }))
    await flush()
    expect(popover()).toBeNull()
    w.unmount()
  })

  it('a pointerdown inside the popover or on the trigger does not close it', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    popover()!.querySelector('[role="tab"]')!.dispatchEvent(
      new PointerEvent('pointerdown', { bubbles: true }),
    )
    await flush()
    expect(popover()).toBeTruthy()
    triggerButton().dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }))
    await flush()
    expect(popover()).toBeTruthy()
    w.unmount()
  })

  it('clicking the trigger again toggles the popover closed', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    await w.get('button').trigger('click')
    await flush()
    expect(popover()).toBeNull()
    w.unmount()
  })

  it('Tab from the last focusable wraps inside the popover (light trap)', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    const buttons = Array.from(popover()!.querySelectorAll<HTMLButtonElement>('button'))
    const last = buttons[buttons.length - 1]
    last.focus()
    expect(document.activeElement).toBe(last)
    popover()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }))
    await flush()
    const firstTab = popover()!.querySelector<HTMLElement>('[role="tab"]')!
    expect(document.activeElement).toBe(firstTab)
    w.unmount()
  })
})

describe('ActivityTrigger badge', () => {
  it('shows the running count with brand tone', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('a', 'running'), shell('b', 'running')] }))
    await activity.fetchNow('s1')
    const w = mountAttached()
    const badge = w.get('.w-icon-btn-badge')
    expect(badge.text()).toBe('2')
    expect(badge.classes()).toContain('w-icon-btn-badge--brand')
    // the accessible name carries the counts
    expect(triggerButton().getAttribute('aria-label')).toContain('2')
    w.unmount()
  })

  it('unviewed failures outrank the running count and switch to danger', async () => {
    api.getActivity.mockResolvedValueOnce(
      snap('s1', { shells: [shell('a', 'running'), shell('dead', 'failed')] }),
    )
    await activity.fetchNow('s1')
    expect(activity.unviewedFailureCountOf('s1')).toBe(1)
    const w = mountAttached()
    const badge = w.get('.w-icon-btn-badge')
    expect(badge.text()).toBe('1')
    expect(badge.classes()).toContain('w-icon-btn-badge--danger')
    w.unmount()
  })

  it('no badge when nothing runs and nothing failed', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountAttached()
    expect(w.find('.w-icon-btn-badge').exists()).toBe(false)
    w.unmount()
  })

  it('opening the panel marks the failures viewed (badge resets)', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('dead', 'failed')] }))
    await activity.fetchNow('s1')
    const w = mountAttached()
    expect(w.get('.w-icon-btn-badge').text()).toBe('1')
    await w.get('button').trigger('click')
    await flush()
    expect(activity.unviewedFailureCountOf('s1')).toBe(0)
    expect(w.find('.w-icon-btn-badge').exists()).toBe(false)
    w.unmount()
  })
})

describe('ActivityTrigger data on open', () => {
  it('demo mode: opening seeds the empty snapshot instead of a perpetual skeleton', async () => {
    activity.setDemo(true)
    const w = mountAttached()
    expect(activity.statusOf('s1')).toBe('loading')
    await w.get('button').trigger('click')
    await flush()
    expect(activity.statusOf('s1')).toBe('ready')
    expect(activity.snapshotOf('s1')).not.toBeNull()
    expect(api.getActivity).not.toHaveBeenCalled()
    expect(popover()!.textContent).toContain(i18n.global.t('activity.backgroundEmpty'))
    w.unmount()
  })

  it('live mode: opening a session with no data schedules a refresh', async () => {
    vi.useFakeTimers()
    try {
      api.getActivity.mockResolvedValueOnce(snap('s1'))
      const w = mountAttached()
      await w.get('button').trigger('click')
      await vi.advanceTimersByTimeAsync(200) // the store's debounce window
      expect(api.getActivity).toHaveBeenCalledWith('s1')
      w.unmount()
    } finally {
      vi.useRealTimers()
    }
  })
})
