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
import { useToastStore } from '../../stores/toast'
import type { ActivitySnapshot, BackgroundShell, CronEntry, WakeState } from '../../types'

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

const cron = (id: string): CronEntry => ({
  id,
  name: `cron-${id}`,
  spec: '*/15 * * * *',
  prompt: 'check',
  created_at: '2026-11-07T00:00:00Z',
  last_fired_at: null,
  next_fire_at: '2026-11-07T00:15:00Z',
})

const wake = (over: Partial<WakeState> = {}): WakeState => ({
  enabled: true,
  capped: false,
  stalled: false,
  pending_notifications: 0,
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

  it('a pointerdown inside a teleported modal (the stop/delete confirms) keeps the panel open', async () => {
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    expect(popover()).toBeTruthy()
    // The confirm dialogs teleport a .w-modal-backdrop to body — visually
    // above the popover and opened from it; interacting there is not an
    // outside click.
    const backdrop = document.createElement('div')
    backdrop.className = 'w-modal-backdrop'
    const action = document.createElement('button')
    backdrop.appendChild(action)
    document.body.appendChild(backdrop)
    action.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }))
    await flush()
    expect(popover()).toBeTruthy()
    backdrop.remove()
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

  it('the trap ignores buttons inside hidden v-show tab panels (regression)', async () => {
    // A cron entry puts a delete button in the SCHEDULED tabpanel, which
    // v-show keeps in the DOM under display:none while Background (the
    // default tab) is active. The trap set must exclude it: with the hidden
    // button counted as "last", Tab from the close button escapes the
    // popover (the browser skips display:none and leaves the dialog).
    api.getActivity.mockResolvedValueOnce(snap('s1', { crons: [cron('c1')] }))
    await activity.fetchNow('s1')
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()

    const pop = popover()!
    const hiddenDelete = pop.querySelector<HTMLButtonElement>('.act-cron-row .act-stop-btn')!
    expect(hiddenDelete).toBeTruthy()
    expect(hiddenDelete.closest<HTMLElement>('.act-tabpanel')!.style.display).toBe('none')

    // Tab from the LAST VISIBLE control (close) wraps to the first tab —
    // the hidden delete button never receives focus.
    const closeBtn = pop.querySelector<HTMLButtonElement>('.act-close')!
    closeBtn.focus()
    pop.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }))
    await flush()
    const firstTab = pop.querySelector<HTMLElement>('[role="tab"]')!
    expect(document.activeElement).toBe(firstTab)
    expect(document.activeElement).not.toBe(hiddenDelete)

    // Shift+Tab from the first tab wraps to the last VISIBLE control (the
    // close button), not to the hidden delete.
    firstTab.focus()
    pop.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, bubbles: true }))
    await flush()
    expect(document.activeElement).toBe(closeBtn)
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

describe('ActivityTrigger wake-cap toast (panel closed)', () => {
  it('toasts once per cap episode for the active session, without opening the panel', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountAttached()
    const toast = useToastStore()
    expect(toast.toasts).toHaveLength(0)

    // The cap rises while the popover is closed — the typical case.
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 3 }) }))
    await activity.fetchNow('s1')
    await flush()
    expect(toast.toasts).toHaveLength(1)
    expect(toast.toasts[0].kind).toBe('warn')
    expect(toast.toasts[0].message).toContain('3')

    // Still capped on the next refresh: same episode, no second toast.
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 3 }) }))
    await activity.fetchNow('s1')
    await flush()
    expect(toast.toasts).toHaveLength(1)

    // A new episode (cap lifted, then hit again) toasts again.
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    await flush()
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 1 }) }))
    await activity.fetchNow('s1')
    await flush()
    expect(toast.toasts.filter((x) => x.kind === 'warn')).toHaveLength(2)
    w.unmount()
  })

  it('mounting into an already-capped session stays silent (no retroactive toast)', async () => {
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 2 }) }))
    await activity.fetchNow('s1')
    const w = mountAttached()
    await flush()
    expect(useToastStore().toasts).toHaveLength(0)
    w.unmount()
  })

  it('does not re-toast when switching away and back to a still-capped session', async () => {
    const sessions = useSessionStore()
    sessions.seed([SESSION, { ...SESSION, id: 's2', title: 'S2' }] as never, {})
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    const w = mountAttached()
    const toast = useToastStore()

    // The cap rises while s1 is active — one toast.
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 2 }) }))
    await activity.fetchNow('s1')
    await flush()
    expect(toast.toasts).toHaveLength(1)

    // Switch away (s2 is uncapped) and back to s1, whose cached snapshot is
    // still capped: same episode, the per-session guard holds — no re-toast.
    api.getActivity.mockResolvedValueOnce(snap('s2'))
    await activity.fetchNow('s2')
    sessions.activeId = 's2'
    await flush()
    sessions.activeId = 's1'
    await flush()
    expect(toast.toasts).toHaveLength(1)

    // The cap lifting re-arms the guard: the next episode toasts again.
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    await flush()
    api.getActivity.mockResolvedValueOnce(snap('s1', { wake: wake({ capped: true, pending_notifications: 1 }) }))
    await activity.fetchNow('s1')
    await flush()
    expect(toast.toasts.filter((x) => x.kind === 'warn')).toHaveLength(2)
    w.unmount()
  })

  it('toasts once when the first snapshot seen for the active session is already capped (kept behaviour)', async () => {
    const sessions = useSessionStore()
    sessions.seed([SESSION, { ...SESSION, id: 's2', title: 'S2' }] as never, {})
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    await activity.fetchNow('s1')
    // s2 is armed capped before it is ever shown (the user was elsewhere).
    api.getActivity.mockResolvedValueOnce(snap('s2', { wake: wake({ capped: true, pending_notifications: 4 }) }))
    await activity.fetchNow('s2')
    const w = mountAttached()
    const toast = useToastStore()

    // Switching onto the still-capped session informs the user — once.
    sessions.activeId = 's2'
    await flush()
    expect(toast.toasts).toHaveLength(1)
    expect(toast.toasts[0].kind).toBe('warn')
    expect(toast.toasts[0].message).toContain('4')

    // Away and back: same episode, no second toast.
    sessions.activeId = 's1'
    await flush()
    sessions.activeId = 's2'
    await flush()
    expect(toast.toasts.filter((x) => x.kind === 'warn')).toHaveLength(1)
    w.unmount()
  })
})

describe('ActivityTrigger "send a message" action', () => {
  it('closes the panel and emits compose from the capped strip', async () => {
    api.getActivity.mockResolvedValueOnce(
      snap('s1', { crons: [cron('c1')], wake: wake({ capped: true, pending_notifications: 1 }) }),
    )
    await activity.fetchNow('s1')
    const w = mountAttached()
    await w.get('button').trigger('click')
    await flush()
    const pop = popover()!
    ;(pop.querySelector('[data-tab="scheduled"]') as HTMLElement).click()
    await flush()
    ;(pop.querySelector('.act-wake-action') as HTMLElement).click()
    await flush()
    expect(popover()).toBeNull()
    expect(w.emitted('compose')).toHaveLength(1)
    w.unmount()
  })
})
