// The merged shells+tasks list: ordering (running pinned on top, terminal
// by ended_at desc), per-status rendering (exit code, stopped_by user vs
// model), stop actions behind the confirm dialog, output expansion driving
// the store watcher, inline action errors, and the aria-live completion
// announcements.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import BackgroundTab from './BackgroundTab.vue'
import { useActivityStore } from '../../stores/activity'
import { useConfirmStore } from '../../stores/confirm'
import type {
  ActivitySnapshot,
  BackgroundShell,
  BackgroundTask,
  OutputTail,
  StoppedBy,
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

const shell = (
  id: string,
  status: BackgroundShell['status'],
  over: Partial<BackgroundShell> = {},
): BackgroundShell => ({
  id,
  command: `$ cmd-${id}`,
  status,
  exit_code: status === 'failed' ? 1 : status === 'running' ? null : 0,
  stopped_by: '',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:01Z',
  output_bytes: 0,
  owner: 'main',
  ...over,
})

const task = (
  id: string,
  status: BackgroundTask['status'],
  stoppedBy: StoppedBy = '',
): BackgroundTask => ({
  id,
  description: `task-${id}`,
  subagent: 'general',
  status,
  stopped_by: stoppedBy,
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:02Z',
})

const snap = (sid: string, over: Partial<ActivitySnapshot> = {}): ActivitySnapshot => ({
  session_id: sid,
  server_time: '2026-11-07T00:00:00Z',
  todos: { items: [], updated_at: null },
  shells: [],
  tasks: [],
  crons: [],
  subagents: [],
  wake: { enabled: true, capped: false, stalled: false, pending_notifications: 0 },
  ...over,
})

const tail = (status: OutputTail['status'], output = 'out'): OutputTail => ({
  id: 'any',
  status,
  output,
  truncated: false,
  total_bytes: output.length,
})

let activity: ReturnType<typeof useActivityStore>
let confirm: ReturnType<typeof useConfirmStore>

async function mountWith(over: Partial<ActivitySnapshot>, sid = 's1') {
  api.getActivity.mockResolvedValueOnce(snap(sid, over))
  await activity.fetchNow(sid)
  return mount(BackgroundTab, { props: { sid }, global: { plugins: [i18n] } })
}

beforeEach(() => {
  vi.useFakeTimers()
  setActivePinia(createPinia())
  // resetAllMocks (not clearAllMocks): a failed action skips its refetch, so
  // its queued mockResolvedValueOnce would otherwise leak into the next test.
  vi.resetAllMocks()
  activity = useActivityStore()
  confirm = useConfirmStore()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('BackgroundTab ordering', () => {
  it('pins running items on top (longest first) and sorts terminal by ended_at desc', async () => {
    const w = await mountWith({
      shells: [
        shell('done-old', 'completed', { ended_at: '2026-11-07T00:00:01Z' }),
        shell('run-b', 'running', { started_at: '2026-11-07T00:00:20Z' }),
        shell('run-a', 'running', { started_at: '2026-11-07T00:00:10Z' }),
        shell('done-new', 'completed', { ended_at: '2026-11-07T00:00:30Z' }),
      ],
      tasks: [task('t-stopped', 'stopped', 'user')],
    })
    const names = w.findAll('.act-row-name').map((x) => x.text())
    // running first (longest started first), then terminal by ended_at desc
    expect(names).toEqual([
      '$ cmd-run-a',
      '$ cmd-run-b',
      '$ cmd-done-new',
      'task-t-stopped',
      '$ cmd-done-old',
    ])
  })
})

describe('BackgroundTab status rendering', () => {
  it('labels each status: running, completed, failed with exit code, stopped by user / by model', async () => {
    const w = await mountWith({
      shells: [
        shell('r1', 'running'),
        shell('ok', 'completed', { exit_code: 0 }),
        shell('bad', 'failed', { exit_code: 127 }),
        shell('killed', 'stopped', { stopped_by: 'user' }),
      ],
      tasks: [task('t1', 'stopped', 'model')],
    })
    const text = w.text()
    expect(text).toContain(i18n.global.t('activity.status.running'))
    expect(text).toContain(i18n.global.t('activity.status.completed'))
    expect(text).toContain(i18n.global.t('activity.status.failedExit', { code: 127 }))
    expect(text).toContain(i18n.global.t('activity.status.stoppedByUser'))
    expect(text).toContain(i18n.global.t('activity.status.stoppedByModel'))
    // type prefixes distinguish shells from subagent tasks
    expect(text).toContain(i18n.global.t('activity.kindShell'))
    expect(text).toContain(i18n.global.t('activity.kindTask'))
  })

  it('renders the empty state when nothing runs or ran', async () => {
    const w = await mountWith({})
    expect(w.get('.act-empty').text()).toContain(i18n.global.t('activity.backgroundEmpty'))
  })
})

describe('BackgroundTab row chrome', () => {
  it('carries the full command/description in the row title; no kind glyph', async () => {
    // The visible name ellipsizes (the command owns the row's space); the
    // untruncated text must still be reachable — the row's title is the
    // repo's tooltip mechanism. The left terminal-prompt glyph is dropped
    // (status is conveyed by the pill); the kind stays as sr-only text.
    const cmd = 'echo tree started; sleep 120 & sleep 120 & wait'
    const w = await mountWith({
      shells: [shell('r1', 'running', { command: cmd })],
      tasks: [task('t1', 'running')],
    })
    const rows = w.findAll('.act-row')
    expect(rows[0].attributes('title')).toBe(cmd)
    expect(rows[1].attributes('title')).toBe('task-t1')
    expect(w.find('.act-row-kind').exists()).toBe(false)
    expect(w.text()).toContain(i18n.global.t('activity.kindShell'))
    expect(w.text()).toContain(i18n.global.t('activity.kindTask'))
  })

  it('the stop control is an icon-only ghost whose accessible name carries the verb', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    const btn = w.get('.act-stop-btn')
    expect(btn.attributes('aria-label')).toBe(i18n.global.t('activity.stop'))
    expect(btn.attributes('title')).toBe(i18n.global.t('activity.stop'))
    // no visible label — the destructive wording lives in the confirm dialog
    expect(btn.text()).toBe('')
  })
})

describe('BackgroundTab stop actions', () => {
  it('offers stop only for running rows (terminal rows have no action)', async () => {
    const w = await mountWith({
      shells: [shell('r1', 'running'), shell('done', 'completed')],
      tasks: [task('t1', 'failed')],
    })
    expect(w.findAll('.act-stop-btn')).toHaveLength(1)
  })

  it('kill shell: confirm cancelled → no request; confirmed → killShell then refetch', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    await w.get('.act-stop-btn').trigger('click')
    expect(confirm.req?.title).toBe(i18n.global.t('activity.confirmStopTitle'))
    // cancel: nothing fires
    confirm.settle(false)
    await flush()
    expect(api.killShell).not.toHaveBeenCalled()

    await w.get('.act-stop-btn').trigger('click')
    api.killShell.mockResolvedValueOnce({ shell: shell('r1', 'stopped'), already_finished: false })
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    confirm.settle(true)
    await flush()
    expect(api.killShell).toHaveBeenCalledWith('s1', 'r1')
    expect(api.getActivity).toHaveBeenCalledTimes(2) // seed + post-action refetch
  })

  it('stop task follows the same confirm path via the task endpoint', async () => {
    const w = await mountWith({ tasks: [task('t1', 'running')] })
    await w.get('.act-stop-btn').trigger('click')
    api.stopTask.mockResolvedValueOnce({ task: task('t1', 'stopped'), already_finished: false })
    api.getActivity.mockResolvedValueOnce(snap('s1'))
    confirm.settle(true)
    await flush()
    expect(api.stopTask).toHaveBeenCalledWith('s1', 't1')
    expect(api.killShell).not.toHaveBeenCalled()
  })

  it('disables the stop button while the kill request is in flight', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    await w.get('.act-stop-btn').trigger('click')
    let resolveKill!: (v: unknown) => void
    api.killShell.mockImplementationOnce(
      () => new Promise((res) => (resolveKill = res)),
    )
    confirm.settle(true)
    await flush()
    expect(w.get('.act-stop-btn').attributes('disabled')).toBeDefined()

    resolveKill({ shell: shell('r1', 'stopped'), already_finished: false })
    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('r1', 'stopped')] }))
    await flush()
    // stopped now: no stop button on a terminal row
    expect(w.find('.act-stop-btn').exists()).toBe(false)
  })

  it('a failed action renders its inline error next to the row', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    await w.get('.act-stop-btn').trigger('click')
    api.killShell.mockRejectedValueOnce(new Error('403 viewer'))
    confirm.settle(true)
    await flush()
    expect(w.get('.act-row-err').text()).toContain('403 viewer')
    // a failed action skips the refetch (store contract)
    expect(api.getActivity).toHaveBeenCalledTimes(1)
  })
})

describe('BackgroundTab output watcher', () => {
  it('expanding starts the tail watcher; collapsing stops it', async () => {
    api.getShellOutput.mockResolvedValue(tail('running', 'chunk'))
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    expect(api.getShellOutput).not.toHaveBeenCalled()

    await w.get('.act-out-toggle').trigger('click')
    await flush()
    expect(api.getShellOutput).toHaveBeenCalledTimes(1) // immediate read
    expect(w.get('.act-output-text').text()).toContain('chunk')

    await vi.advanceTimersByTimeAsync(4000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(3) // +2s +2s ticks

    await w.get('.act-out-toggle').trigger('click') // collapse unmounts OutputTail
    await vi.advanceTimersByTimeAsync(4000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(3) // watcher stopped
  })

  it('shows the truncation note for a truncated tail', async () => {
    api.getShellOutput.mockResolvedValue({ ...tail('running', 'xyz'), truncated: true, total_bytes: 9000 })
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    await w.get('.act-out-toggle').trigger('click')
    await flush()
    expect(w.get('.act-output-note').text()).toContain('9 KB')
  })
})

describe('BackgroundTab announcements', () => {
  it('announces running → failed through the polite live region', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    expect(w.get('[aria-live="polite"]').text()).toBe('')

    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('r1', 'failed', { exit_code: 2 })] }))
    await activity.fetchNow('s1')
    await flush()
    expect(w.get('[aria-live="polite"]').text()).toBe(
      i18n.global.t('activity.liveFailed', { name: '$ cmd-r1', code: 2 }),
    )
  })

  it('announces running → completed', async () => {
    const w = await mountWith({ shells: [shell('r1', 'running')] })
    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('r1', 'completed')] }))
    await activity.fetchNow('s1')
    await flush()
    expect(w.get('[aria-live="polite"]').text()).toBe(
      i18n.global.t('activity.liveFinished', { name: '$ cmd-r1' }),
    )
  })
})

describe('BackgroundTab initiator column', () => {
  it('renders an origin badge per row with the act-origin testid; main stays quiet', async () => {
    const w = await mountWith({
      shells: [
        shell('m1', 'completed'),
        shell('s2', 'completed', { owner: 'subagent:researcher#call-a1b2' }),
      ],
    })
    const main = w.get('[data-testid="act-origin-m1"]')
    expect(main.text()).toBe(i18n.global.t('chat.sourceMain'))
    expect(main.classes()).toContain('main')
    // Subagent rows carry the def name plus the 4-char short code.
    // the badge's CSS uppercases visually; the DOM text stays raw
    expect(w.get('[data-testid="act-origin-s2"]').text()).toBe('researcher#a1b2')
  })

  it('the initiator header opens a multi-select filter over the origins present', async () => {
    const w = await mountWith({
      shells: [
        shell('m1', 'completed'),
        shell('s2', 'completed', { owner: 'subagent:researcher#call-a1b2' }),
        shell('s3', 'completed', { owner: 'subagent:researcher#call-c3d4' }),
      ],
    })
    expect(w.find('.act-origin-menu').exists()).toBe(false)
    await w.get('[data-testid="initiator-col"]').trigger('click')
    expect(w.get('[data-testid="initiator-col"]').attributes('aria-expanded')).toBe('true')
    // Options: main once + the researcher def once (calls fold into defs).
    const opts = w.findAll('.act-origin-menu input[type="checkbox"]')
    expect(opts).toHaveLength(2)
    // Selecting only the subagent origin hides the main shell.
    await opts[1].setValue(true)
    expect(w.find('[data-testid="act-origin-m1"]').exists()).toBe(false)
    expect(w.findAll('.act-row')).toHaveLength(2)
    // Multi-select: adding main brings every row back.
    await opts[0].setValue(true)
    expect(w.findAll('.act-row')).toHaveLength(3)
    // Clearing the selection restores the unfiltered list.
    await opts[0].setValue(false)
    await opts[1].setValue(false)
    expect(w.findAll('.act-row')).toHaveLength(3)
  })

  it('a selection whose rows all vanished shows the no-match note, not the empty state', async () => {
    const w = await mountWith({
      shells: [shell('m1', 'completed'), shell('s2', 'completed', { owner: 'subagent:researcher#call-a1b2' })],
    })
    await w.get('[data-testid="initiator-col"]').trigger('click')
    const opts = w.findAll('.act-origin-menu input[type="checkbox"]')
    await opts[1].setValue(true) // researcher only
    expect(w.find('[data-testid="act-origin-m1"]').exists()).toBe(false)
    // The child's record drops out of the snapshot (pruned); the selection
    // now matches nothing — a note, never the session-level empty state.
    api.getActivity.mockResolvedValueOnce(snap('s1', { shells: [shell('m1', 'completed')] }))
    await activity.fetchNow('s1')
    await flush()
    expect(w.find('.act-row').exists()).toBe(false)
    expect(w.get('[data-testid="bg-no-match"]').text()).toBe(
      i18n.global.t('activity.initiatorNoMatch'),
    )
  })
})
