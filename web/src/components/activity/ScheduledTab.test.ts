// Scheduled prompts: cron fields (name / spec / prompt), wake-state
// explanations (capped / stalled / disabled / pending), delete behind the
// confirm dialog (cancel → no request), inline action errors, empty state.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import ScheduledTab from './ScheduledTab.vue'
import { useActivityStore } from '../../stores/activity'
import { useConfirmStore } from '../../stores/confirm'
import type { ActivitySnapshot, CronEntry, WakeState } from '../../types'

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

const cron = (id: string, over: Partial<CronEntry> = {}): CronEntry => ({
  id,
  name: `cron-${id}`,
  spec: '*/15 * * * *',
  prompt: 'check the build',
  created_at: '2026-11-07T00:00:00Z',
  last_fired_at: null,
  next_fire_at: '2026-11-07T00:15:00Z',
  ...over,
})

const wake = (over: Partial<WakeState> = {}): WakeState => ({
  enabled: true,
  capped: false,
  stalled: false,
  pending_notifications: 0,
  ...over,
})

function snapWith(crons: CronEntry[], w: WakeState): ActivitySnapshot {
  return {
    session_id: 's1',
    server_time: '2026-11-07T00:00:00Z',
    todos: { items: [], updated_at: null },
    shells: [],
    tasks: [],
    subagents: [],
    crons,
    wake: w,
  }
}

let activity: ReturnType<typeof useActivityStore>
let confirm: ReturnType<typeof useConfirmStore>

async function mountWith(crons: CronEntry[], w = wake()) {
  api.getActivity.mockResolvedValueOnce(snapWith(crons, w))
  await activity.fetchNow('s1')
  return mount(ScheduledTab, { props: { sid: 's1' }, global: { plugins: [i18n] } })
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.resetAllMocks()
  activity = useActivityStore()
  confirm = useConfirmStore()
})

describe('ScheduledTab cron entries', () => {
  it('renders name, spec, prompt and fire-time labels', async () => {
    // last_fired null / next_fire null keep the assertions locale-stable
    // (formatted dates vary by environment; fmtDateTime is covered by lib
    // tests-free usage — here only the fixed label keys matter).
    const w = await mountWith([cron('c1', { last_fired_at: null, next_fire_at: null })])
    const row = w.get('.act-cron-row')
    expect(row.text()).toContain('cron-c1')
    expect(row.text()).toContain('*/15 * * * *')
    expect(row.text()).toContain('check the build')
    expect(row.text()).toContain(i18n.global.t('activity.cronNever'))
    expect(row.text()).toContain(i18n.global.t('activity.cronNoNext'))
  })

  it('renders the empty state', async () => {
    const w = await mountWith([])
    expect(w.get('.act-empty').text()).toContain(i18n.global.t('activity.scheduledEmpty'))
  })

  it('renders the spec as plain mono text and the prompt with a full-text title', async () => {
    // The schedule is data (code-styled text), not chip chrome that reads as
    // a clickable tab; the prompt value ellipsizes and its title keeps the
    // full text reachable.
    const w = await mountWith([cron('c1', { prompt: 'Summarize progress in one line.' })])
    const spec = w.get('.act-cron-spec')
    expect(spec.element.tagName).toBe('CODE')
    expect(spec.text()).toBe('*/15 * * * *')
    const prompt = w.get('.act-cron-prompt')
    expect(prompt.text()).toBe('Summarize progress in one line.')
    expect(prompt.attributes('title')).toBe('Summarize progress in one line.')
    // the label is its own muted caption, not part of the value
    expect(w.get('.act-cron-prompt-label').text()).toBe(i18n.global.t('activity.cronPrompt'))
  })
})

describe('ScheduledTab wake explanations', () => {
  it('capped shows the queued-notifications warning with a "send a message" action', async () => {
    const w = await mountWith([cron('c1')], wake({ capped: true, pending_notifications: 2 }))
    expect(w.get('.act-wake.warn').text()).toContain(
      i18n.global.t('activity.wakeCapped', { count: 2 }),
    )
    const action = w.get('.act-wake-action')
    expect(action.text()).toBe(i18n.global.t('activity.wakeCappedAction'))
    await action.trigger('click')
    expect(w.emitted('compose')).toHaveLength(1)
  })

  it('stalled and disabled each get a readable line', async () => {
    const w = await mountWith([], wake({ stalled: true, enabled: false }))
    const lines = w.findAll('.act-wake').map((x) => x.text())
    expect(lines.join(' ')).toContain(i18n.global.t('activity.wakeStalled'))
    expect(lines.join(' ')).toContain(i18n.global.t('activity.wakeDisabled'))
  })

  it('pending notifications (not capped) render the pending line', async () => {
    const w = await mountWith([], wake({ pending_notifications: 3 }))
    expect(w.get('.act-wake').text()).toContain(
      i18n.global.t('activity.wakePending', { count: 3 }),
    )
  })
})

describe('ScheduledTab delete', () => {
  it('delete confirm cancelled → no request; confirmed → deleteCron then refetch', async () => {
    const w = await mountWith([cron('c1')])
    await w.get('.act-stop-btn').trigger('click')
    expect(confirm.req?.title).toBe(i18n.global.t('activity.confirmDeleteTitle'))
    confirm.settle(false)
    await flush()
    expect(api.deleteCron).not.toHaveBeenCalled()

    await w.get('.act-stop-btn').trigger('click')
    api.deleteCron.mockResolvedValueOnce({ deleted: true, id: 'c1' })
    api.getActivity.mockResolvedValueOnce(snapWith([], wake()))
    confirm.settle(true)
    await flush()
    expect(api.deleteCron).toHaveBeenCalledWith('s1', 'c1')
    expect(api.getActivity).toHaveBeenCalledTimes(2)
  })

  it('a failed delete renders its inline error', async () => {
    const w = await mountWith([cron('c1')])
    await w.get('.act-stop-btn').trigger('click')
    api.deleteCron.mockRejectedValueOnce(new Error('403 viewer'))
    confirm.settle(true)
    await flush()
    expect(w.get('.act-row-err').text()).toContain('403 viewer')
  })
})
