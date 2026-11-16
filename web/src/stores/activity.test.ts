// The session activity store: per-session snapshot caching, signal-driven
// debounced refresh (active session only), deployment-unavailable (501)
// detection, REST actions with post-action refetch, unviewed-failure badge
// bookkeeping, output-tail polling, D2 delete-confirm counts — plus its
// wiring into the session lifecycle (setActive, global stream, delete).
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  // activity surface
  getActivity: vi.fn(),
  getShellOutput: vi.fn(),
  getTaskOutput: vi.fn(),
  killShell: vi.fn(),
  stopTask: vi.fn(),
  deleteCron: vi.fn(),
  // sessions-store surface touched by the wiring tests
  listSessions: vi.fn(),
  createSession: vi.fn(),
  createRun: vi.fn(),
  getSession: vi.fn(),
  getGoal: vi.fn(),
  getSessionContext: vi.fn(),
  deleteSession: vi.fn(),
  streamEvents: vi.fn(),
  streamRun: vi.fn(),
}))
vi.mock('../api/client', () => ({ ...api }))

import { useActivityStore } from './activity'
import { useSessionStore } from './sessions'
import { ApiError } from '../api/transport'
import type {
  ActivitySnapshot,
  BackgroundShell,
  BackgroundTask,
  CronEntry,
  OutputTail,
  TodoItem,
} from '../types'

// Microtask-only flush (fake timers make setTimeout-based waits unusable).
const flush = async () => {
  for (let i = 0; i < 5; i++) await Promise.resolve()
}

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

const shell = (id: string, status: BackgroundShell['status']): BackgroundShell => ({
  id,
  command: `cmd-${id}`,
  status,
  exit_code: status === 'running' ? null : 0,
  stopped_by: '',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:01Z',
  output_bytes: 0,
  owner: 'main',
})

const task = (id: string, status: BackgroundTask['status']): BackgroundTask => ({
  id,
  description: `task-${id}`,
  subagent: 'general',
  status,
  stopped_by: '',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: status === 'running' ? null : '2026-11-07T00:00:01Z',
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

const tail = (status: OutputTail['status'], output = 'tail'): OutputTail => ({
  id: 'sh1',
  status,
  output,
  truncated: false,
  total_bytes: output.length,
})

const setHidden = (v: boolean) => {
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(v)
}

beforeEach(() => {
  vi.useFakeTimers()
  setActivePinia(createPinia())
  vi.clearAllMocks()
  api.getGoal.mockResolvedValue({ goal: null, running: false })
  api.getSessionContext.mockResolvedValue({})
  api.getSession.mockResolvedValue({ messages: [] })
  api.streamEvents.mockReturnValue({ close: () => {} })
})

describe('snapshot caching and signal-driven refresh', () => {
  it('coalesces signals inside the debounce window into one GET', async () => {
    api.getActivity.mockResolvedValue(snap('a'))
    const m = useActivityStore()
    m.refresh('a')
    m.refresh('a')
    m.refresh('a')
    await vi.advanceTimersByTimeAsync(199)
    expect(api.getActivity).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(api.getActivity).toHaveBeenCalledTimes(1)
    expect(m.statusOf('a')).toBe('ready')
  })

  it('a sustained signal burst cannot postpone the fetch past the max-wait cap', async () => {
    api.getActivity.mockResolvedValue(snap('a'))
    const m = useActivityStore()
    m.refresh('a') // t=0 opens the window
    // A signal every 150ms — faster than the 200ms window — for a full 3s:
    // a pure trailing debounce would re-arm forever and never fetch.
    let landedWithinCap = false
    for (let t = 150; t <= 3000; t += 150) {
      await vi.advanceTimersByTimeAsync(150)
      m.refresh('a')
      if (t <= 1200 && api.getActivity.mock.calls.length >= 1) landedWithinCap = true
    }
    expect(landedWithinCap).toBe(true) // ≥1 fetch landed ≤ ~1.2s into the burst
    // The cap repeats for the rest of the burst and a trailing edge closes
    // the last window: several fetches over 3s of signals, never one per
    // signal (21 signals → 3-4 GETs).
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(api.getActivity.mock.calls.length).toBeLessThanOrEqual(4)
    expect(m.statusOf('a')).toBe('ready')
  })

  it('without a burst a single signal still fetches once after the window', async () => {
    api.getActivity.mockResolvedValue(snap('a'))
    const m = useActivityStore()
    m.refresh('a')
    await vi.advanceTimersByTimeAsync(199)
    expect(api.getActivity).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(api.getActivity).toHaveBeenCalledTimes(1)
    // The cap only bounds a PENDING window — it never fires on its own.
    await vi.advanceTimersByTimeAsync(5000)
    expect(api.getActivity).toHaveBeenCalledTimes(1)
  })

  it('caches per session: switching keeps each session data isolated', async () => {
    api.getActivity.mockImplementation(async (sid: string) =>
      snap(sid, { shells: [shell(`${sid}-1`, 'running')] }),
    )
    const m = useActivityStore()
    m.refresh('a')
    m.refresh('b')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(2)
    expect(m.snapshotOf('a')?.shells[0].id).toBe('a-1')
    expect(m.snapshotOf('b')?.shells[0].id).toBe('b-1')
  })

  it('an unseen session renders loading, never an empty list', () => {
    const m = useActivityStore()
    expect(m.statusOf('nope')).toBe('loading')
    expect(m.snapshotOf('nope')).toBeNull()
    expect(m.runningCountOf('nope')).toBe(0)
  })

  it('activity_changed fetches the ACTIVE session only; others go stale', async () => {
    api.getActivity.mockResolvedValue(snap('b'))
    const m = useActivityStore()
    m.refresh('b')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(1)

    m.onActivityChanged({ kind: 'shells', session_id: 'b' }, 'a')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(1) // no GET for the inactive session
    expect(m.stateOf('b').stale).toBe(true)

    api.getActivity.mockResolvedValue(snap('a'))
    m.onActivityChanged({ kind: 'todos', session_id: 'a' }, 'a')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(2)
    expect(api.getActivity).toHaveBeenLastCalledWith('a')

    // A successful refetch clears the stale flag.
    api.getActivity.mockResolvedValue(snap('b'))
    await m.fetchNow('b')
    expect(m.stateOf('b').stale).toBe(false)
  })

  it('wake_capped refetches the active session (AC25)', async () => {
    api.getActivity.mockResolvedValue(snap('a'))
    const m = useActivityStore()
    m.onWakeCapped({ session: 'a', queued: 2 }, 'a')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledWith('a')
  })

  it('stream (re)open refetches immediately and cancels a pending debounce', async () => {
    api.getActivity.mockResolvedValue(snap('a'))
    const m = useActivityStore()
    m.refresh('a') // still inside the debounce window…
    m.onStreamOpen('a')
    await flush()
    expect(api.getActivity).toHaveBeenCalledTimes(1) // …but open flushed it now
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(1) // the queued timer was dropped
  })

  it('fetch failures land in the explicit error state with the snapshot kept', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.fetchNow('a')
    api.getActivity.mockRejectedValueOnce(new Error('net down'))
    await m.fetchNow('a')
    const st = m.stateOf('a')
    expect(st.error).toBe('net down')
    expect(st.loading).toBe(false)
    expect(st.snapshot).not.toBeNull() // stale-while-revalidate context
    expect(m.statusOf('a')).toBe('error')
  })

  it('S-B-0501 flips to unavailable (never an empty list) and stops re-probing', async () => {
    const m = useActivityStore()
    api.getActivity.mockRejectedValueOnce(new ApiError('load failed: unavailable', 'S-B-0501'))
    await m.fetchNow('a')
    const st = m.stateOf('a')
    expect(st.unavailable).toBe(true)
    expect(st.snapshot).toBeNull()
    expect(st.error).toBe('') // unavailable is its own state, not an error
    expect(m.statusOf('a')).toBe('unavailable')

    m.refresh('a')
    m.onStreamOpen('a')
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(1) // sticky per deployment
    await expect(m.runningCountsFor('a')).resolves.toBeNull()
  })

  it('demo mode serves empty snapshots without any request', async () => {
    const m = useActivityStore()
    m.setDemo(true)
    m.refresh('a')
    expect(m.statusOf('a')).toBe('ready')
    expect(m.snapshotOf('a')).toEqual(
      expect.objectContaining({ session_id: 'a', shells: [], tasks: [], crons: [] }),
    )
    expect(api.getActivity).not.toHaveBeenCalled()
    await expect(m.runningCountsFor('a')).resolves.toEqual({ shells: 0, tasks: 0, crons: 0 })
    m.watchOutput('a', 'shell', 'sh1')
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).not.toHaveBeenCalled()
  })
})

describe('user actions', () => {
  it('killShell awaits the REST call then refetches (already_finished included)', async () => {
    const m = useActivityStore()
    api.killShell.mockResolvedValueOnce({ shell: shell('sh1', 'stopped'), already_finished: true })
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.killShell('a', 'sh1')
    expect(api.killShell).toHaveBeenCalledWith('a', 'sh1')
    expect(api.getActivity).toHaveBeenCalledWith('a')
    expect(api.killShell.mock.invocationCallOrder[0]).toBeLessThan(
      api.getActivity.mock.invocationCallOrder[0],
    )
    expect(m.stateOf('a').error).toBe('') // already_finished is a 200, not an error
  })

  it('stopTask and deleteCron follow the same call-then-refetch contract', async () => {
    const m = useActivityStore()
    api.stopTask.mockResolvedValueOnce({ task: task('t1', 'stopped'), already_finished: false })
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.stopTask('a', 't1')
    expect(api.stopTask).toHaveBeenCalledWith('a', 't1')
    expect(api.getActivity).toHaveBeenCalledTimes(1)

    api.deleteCron.mockResolvedValueOnce({ deleted: true, id: 'c1' })
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.deleteCron('a', 'c1')
    expect(api.deleteCron).toHaveBeenCalledWith('a', 'c1')
    expect(api.getActivity).toHaveBeenCalledTimes(2)
  })

  it('action failures write the item inline error slot, not the session error', async () => {
    const m = useActivityStore()
    api.stopTask.mockRejectedValueOnce(new Error('403 viewer'))
    await m.stopTask('a', 't1')
    expect(m.stateOf('a').error).toBe('') // fetch errors own the session channel
    expect(m.actionErrorOf('a', 'task', 't1')).toBe('403 viewer')
    expect(api.getActivity).not.toHaveBeenCalled() // failed action skips refetch

    // A successful retry clears the inline slot.
    api.stopTask.mockResolvedValueOnce({ task: task('t1', 'stopped'), already_finished: false })
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.stopTask('a', 't1')
    expect(m.actionErrorOf('a', 'task', 't1')).toBe('')
    expect(api.getActivity).toHaveBeenCalledTimes(1)
  })
})

describe('panel getters', () => {
  it('derives running count, tab counts and todo progress from the snapshot', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(
      snap('a', {
        shells: [shell('s1', 'running'), shell('s2', 'completed')],
        tasks: [task('t1', 'running'), task('t2', 'failed')],
        crons: [cron('c1')],
        todos: {
          items: [todo('one', 'completed'), todo('two', 'completed'), todo('three', 'in_progress'), todo('four', 'pending')],
          updated_at: '2026-11-07T00:00:00Z',
        },
      }),
    )
    await m.fetchNow('a')
    expect(m.runningCountOf('a')).toBe(2) // running shells + running tasks
    expect(m.backgroundCountOf('a')).toBe(4) // every shell + task
    expect(m.cronCountOf('a')).toBe(1)
    expect(m.todoCountOf('a')).toBe(4)
    expect(m.todoProgressOf('a')).toEqual({
      done: 2,
      total: 4,
      current: { content: 'three', status: 'in_progress' },
    })
  })

  it('tracks unviewed failures: new failures count until markViewed, then reset', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(snap('a', { shells: [shell('s1', 'failed')] }))
    await m.fetchNow('a')
    expect(m.unviewedFailureCountOf('a')).toBe(1)

    m.markViewed('a')
    expect(m.unviewedFailureCountOf('a')).toBe(0)

    // A second failure after viewing counts again…
    api.getActivity.mockResolvedValueOnce(
      snap('a', { shells: [shell('s1', 'failed'), shell('s2', 'failed')] }),
    )
    await m.fetchNow('a')
    expect(m.unviewedFailureCountOf('a')).toBe(1)

    // …and a viewed failure disappearing does not resurrect the count (the
    // viewed baseline only grows on markViewed; s2 stays unviewed until then).
    api.getActivity.mockResolvedValueOnce(snap('a', { shells: [shell('s1', 'failed')] }))
    await m.fetchNow('a')
    expect(m.unviewedFailureCountOf('a')).toBe(0)
    expect(m.unviewedFailureCountOf('a')).toBe(m.unviewedFailures['a'])
  })
})

describe('output polling', () => {
  it('fetches the tail immediately, then every 2s while the item runs', async () => {
    const m = useActivityStore()
    api.getShellOutput.mockResolvedValue(tail('running', 'chunk'))
    const w = m.watchOutput('a', 'shell', 'sh1', 8192)
    await flush()
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)
    expect(api.getShellOutput).toHaveBeenLastCalledWith('a', 'sh1', 8192)
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(4000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(4)
    expect(m.outputOf('a', 'shell', 'sh1')?.output).toBe('chunk')

    w.stop() // row collapsed
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(4)
  })

  it('routes shell vs task tails to their own endpoints', async () => {
    const m = useActivityStore()
    api.getTaskOutput.mockResolvedValue(tail('running'))
    const w = m.watchOutput('a', 'task', 't1')
    await flush()
    expect(api.getTaskOutput).toHaveBeenCalledWith('a', 't1', 65536)
    w.stop()
  })

  it('stops once the item reports a terminal status', async () => {
    const m = useActivityStore()
    api.getShellOutput.mockResolvedValue(tail('completed'))
    m.watchOutput('a', 'shell', 'sh1')
    await flush()
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)
  })

  it('skips ticks while the page is hidden and resumes when visible', async () => {
    const m = useActivityStore()
    setHidden(true)
    api.getShellOutput.mockResolvedValue(tail('running'))
    m.watchOutput('a', 'shell', 'sh1')
    await flush()
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).not.toHaveBeenCalled()

    setHidden(false)
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)
  })

  it('a failed tail read stops the watcher and records the error (no retry ladder)', async () => {
    const m = useActivityStore()
    api.getShellOutput.mockRejectedValueOnce(new Error('tail read failed'))
    m.watchOutput('a', 'shell', 'sh1')
    await flush()
    expect(m.outputErrorOf('a', 'shell', 'sh1')).toBe('tail read failed')
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)
  })

  it('re-watching the same key stops the previous watcher (no orphan interval)', async () => {
    const m = useActivityStore()
    api.getShellOutput.mockResolvedValue(tail('running'))
    const w1 = m.watchOutput('a', 'shell', 'sh1')
    await flush()
    expect(api.getShellOutput).toHaveBeenCalledTimes(1)

    // A second watcher for the same item supersedes the first: only one
    // interval keeps polling, and the orphaned handle is a no-op.
    m.watchOutput('a', 'shell', 'sh1')
    await flush()
    expect(api.getShellOutput).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(3) // one tick, not two

    w1.stop() // the superseded handle must not stop the live watcher
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(4)
  })
})

describe('delete-confirm counts (D2) and forget', () => {
  it('serves counts from a fresh cache without refetching', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(
      snap('a', { shells: [shell('s1', 'running')], crons: [cron('c1')] }),
    )
    await m.fetchNow('a')
    api.getActivity.mockClear()
    await expect(m.runningCountsFor('a')).resolves.toEqual({ shells: 1, tasks: 0, crons: 1 })
    expect(api.getActivity).not.toHaveBeenCalled()
  })

  it('fetches on demand for an uncached or stale session', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(snap('a', { tasks: [task('t1', 'running')] }))
    await expect(m.runningCountsFor('a')).resolves.toEqual({ shells: 0, tasks: 1, crons: 0 })

    // Stale cache (a signal arrived while inactive) is re-read, not trusted.
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.fetchNow('a')
    m.onActivityChanged({ kind: 'wake', session_id: 'a' }, 'other')
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await expect(m.runningCountsFor('a')).resolves.toEqual({ shells: 0, tasks: 0, crons: 0 })
    expect(api.getActivity).toHaveBeenCalledTimes(3)
  })

  it('returns null when the read fails (the error stays in the session state)', async () => {
    const m = useActivityStore()
    api.getActivity.mockRejectedValueOnce(new Error('boom'))
    await expect(m.runningCountsFor('a')).resolves.toBeNull()
    expect(m.stateOf('a').error).toBe('boom')
  })

  it('forget drops the session, cancels its pending debounce and stops watchers', async () => {
    const m = useActivityStore()
    api.getActivity.mockResolvedValue(snap('a'))
    await m.fetchNow('a')
    api.getShellOutput.mockResolvedValue(tail('running'))
    m.watchOutput('a', 'shell', 'sh1')
    await flush()

    m.refresh('a') // queued behind the 200ms window…
    m.forget('a')
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.getActivity).toHaveBeenCalledTimes(1) // …never fired
    expect(api.getShellOutput).toHaveBeenCalledTimes(1) // watcher stopped too
    expect(m.snapshotOf('a')).toBeNull()
    expect(m.statusOf('a')).toBe('loading')
    expect(m.unviewedFailureCountOf('a')).toBe(0)
  })

  it('a fetch resolving after forget never resurrects the session (ghost entry)', async () => {
    const m = useActivityStore()
    let resolveFetch: (s: ActivitySnapshot) => void = () => {}
    api.getActivity.mockImplementationOnce(
      () => new Promise<ActivitySnapshot>((res) => (resolveFetch = res)),
    )
    const pending = m.fetchNow('a')
    m.forget('a')
    resolveFetch(snap('a', { shells: [shell('s1', 'running')] }))
    await pending
    expect(m.snapshotOf('a')).toBeNull()
    expect(m.statusOf('a')).toBe('loading') // unseen session, not a ghost cache

    // Same contract for the error path (no ghost error state either).
    let rejectFetch: (e: Error) => void = () => {}
    api.getActivity.mockImplementationOnce(
      () => new Promise<ActivitySnapshot>((_, rej) => (rejectFetch = rej)),
    )
    const failing = m.fetchNow('b')
    m.forget('b')
    rejectFetch(new Error('late failure'))
    await failing
    expect(m.stateOf('b').error).toBe('')

    // A deliberate refresh re-interests the store: the marker is cleared.
    api.getActivity.mockResolvedValueOnce(snap('a'))
    m.refresh('a')
    await vi.advanceTimersByTimeAsync(200)
    expect(m.statusOf('a')).toBe('ready')
  })

  it('forget also clears inline action errors', async () => {
    const m = useActivityStore()
    api.killShell.mockRejectedValueOnce(new Error('403'))
    await m.killShell('a', 'sh1')
    expect(m.actionErrorOf('a', 'shell', 'sh1')).toBe('403')
    m.forget('a')
    expect(m.actionErrorOf('a', 'shell', 'sh1')).toBe('')
  })
})

describe('session lifecycle wiring', () => {
  it('setActive refreshes the switched-to session (AC22)', async () => {
    const sessions = useSessionStore()
    sessions.seed(
      [
        { id: 'a', title: 'A', updatedAt: '', loaded: true, workspace: '', workspaceName: '' },
        { id: 'b', title: 'B', updatedAt: '', loaded: false, workspace: '', workspaceName: '' },
      ] as never,
      {},
    )
    api.getActivity.mockResolvedValue(snap('b'))
    await sessions.setActive('b')
    expect(api.getActivity).not.toHaveBeenCalled() // debounced…
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledWith('b')
  })

  it('the global stream dispatches activity signals into the store (AC20/21/25)', async () => {
    let handlers: Record<string, (...args: unknown[]) => void> = {}
    api.streamEvents.mockImplementation((h: Record<string, (...args: unknown[]) => void>) => {
      handlers = h
      return { close: () => {} }
    })
    api.listSessions.mockResolvedValue([{ id: 's1', title: 'T', updated_at: '' }])
    const sessions = useSessionStore()
    await sessions.init()
    expect(api.streamEvents).toHaveBeenCalledTimes(1)

    const m = useActivityStore()
    api.getActivity.mockResolvedValue(snap('s1'))
    handlers.onOpen()
    await flush()
    expect(api.getActivity).toHaveBeenCalledWith('s1')

    handlers.onActivityChanged({ kind: 'shells', session_id: 's1' })
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(2)

    handlers.onWakeCapped({ session: 's1', queued: 3 })
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(3)

    // Another session's signal never fetches.
    handlers.onActivityChanged({ kind: 'todos', session_id: 'sX' })
    await vi.advanceTimersByTimeAsync(200)
    expect(api.getActivity).toHaveBeenCalledTimes(3)
    expect(api.getActivity).not.toHaveBeenCalledWith('sX', expect.anything())
  })

  it('stream handler errors never throw: they land in the explicit error state', async () => {
    let handlers: Record<string, (...args: unknown[]) => void> = {}
    api.streamEvents.mockImplementation((h: Record<string, (...args: unknown[]) => void>) => {
      handlers = h
      return { close: () => {} }
    })
    api.listSessions.mockResolvedValue([{ id: 's1', title: 'T', updated_at: '' }])
    const sessions = useSessionStore()
    await sessions.init()
    const m = useActivityStore()

    api.getActivity.mockRejectedValueOnce(new Error('flap'))
    expect(() => handlers.onOpen()).not.toThrow()
    await flush()
    expect(m.stateOf('s1').error).toBe('flap')
    expect(m.statusOf('s1')).toBe('error')
  })

  it('removeSession returns the teardown report, forgets activity and clears runAuto', async () => {
    const sessions = useSessionStore()
    sessions.seed(
      [
        { id: 'a', title: 'A', updatedAt: '', loaded: true, workspace: '', workspaceName: '' },
        { id: 'b', title: 'B', updatedAt: '', loaded: true, workspace: '', workspaceName: '' },
      ] as never,
      {},
    )
    const m = useActivityStore()
    api.getActivity.mockResolvedValueOnce(snap('a'))
    await m.fetchNow('a')
    sessions.runAuto['a'] = true

    const report = {
      deleted: true,
      shells_killed: 2,
      tasks_stopped: 1,
      crons_deleted: 3,
      todos_dropped: true,
    }
    api.deleteSession.mockResolvedValueOnce(report)
    await expect(sessions.removeSession('a')).resolves.toEqual(report)
    expect(api.deleteSession).toHaveBeenCalledWith('a')
    expect(m.snapshotOf('a')).toBeNull() // activity.forget ran
    expect(sessions.runAuto['a']).toBeUndefined()
  })

  it('runAuto is set from run_started.auto and cleared on done/error (AC26)', async () => {
    const sessions = useSessionStore()
    sessions.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }] as never,
      {},
    )
    let h: Record<string, (...args: unknown[]) => void> = {}
    api.createRun.mockResolvedValue({ run_id: 'r1', state: 'running', queue_position: 0 })
    api.streamRun.mockImplementation((_rid: string, handlers: Record<string, (...args: unknown[]) => void>) => {
      h = handlers
      return new Promise<void>(() => {})
    })
    const sent = sessions.send('go')
    await flush()
    expect(h.onRunStarted).toBeTruthy()

    h.onRunStarted({ auto: true })
    expect(sessions.runAuto['s1']).toBe(true)
    h.onDone(false)
    expect(sessions.runAuto['s1']).toBeUndefined()

    h.onRunStarted({}) // a user-run has no auto flag
    expect(sessions.runAuto['s1']).toBe(false)
    h.onError('boom')
    expect(sessions.runAuto['s1']).toBeUndefined()
    void sent
  })
})
