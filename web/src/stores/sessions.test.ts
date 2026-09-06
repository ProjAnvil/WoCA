import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  createRun: vi.fn(),
  streamRun: vi.fn(),
  cancelRun: vi.fn(),
  queueRunInput: vi.fn(),
}))

vi.mock('../api/client', () => ({
  ...api,
  getSessionContext: vi.fn(),
  getGoal: vi.fn(),
}))

import { useSessionStore } from './sessions'

interface AttachedStream {
  signal?: AbortSignal
  handlers: {
    onReasoning?: (text: string) => void
    onGoalNote?: (text: string) => void
    onRunStarted?: () => void
  }
}

function seedStore() {
  setActivePinia(createPinia())
  const store = useSessionStore()
  store.seed(
    [{ id: 'session-1', title: 'Session', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    { 'session-1': [] },
  )
  return store
}

describe('session stream cancellation ownership', () => {
  let streams: AttachedStream[]

  beforeEach(() => {
    streams = []
    api.createRun.mockReset().mockResolvedValue({
      run_id: 'run-1',
      state: 'running',
      queue_position: 0,
    })
    api.cancelRun.mockReset().mockResolvedValue(undefined)
    api.queueRunInput.mockReset().mockResolvedValue({ ok: true, queued: 1 })
    api.streamRun.mockReset().mockImplementation((_runID, handlers, signal?: AbortSignal) => {
      streams.push({ signal, handlers })
      return new Promise<void>((resolve) => {
        signal?.addEventListener(
          'abort',
          () => resolve(),
          { once: true },
        )
      })
    })
  })

  it('aborts a prior duplicate attachment without turning it into a run error', async () => {
    const store = seedStore()
    const first = store.send('first')
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    streams[0].handlers.onReasoning?.('old')

    const second = store.send('second')
    await vi.waitFor(() => expect(streams).toHaveLength(2))
    streams[1].handlers.onReasoning?.('new')

    expect(streams[0].signal?.aborted).toBe(true)
    expect(streams[1].signal?.aborted).toBe(false)
    await first
    expect(store.activeError).toBe('')
    const reasoning = store.activeItems.filter((item) => item.kind === 'reasoning')
    expect(reasoning).toHaveLength(1)
    expect(reasoning[0]).toMatchObject({ content: 'oldnew', streaming: true })

    store.cancel()
    await second
    await vi.waitFor(() => expect(store.activeStatus).toBe('done'))
  })

  it('aborts the owned stream immediately on user cancel and cleans up AbortError', async () => {
    const store = seedStore()
    const sending = store.send('cancel me')
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    streams[0].handlers.onReasoning?.('thinking')
    streams[0].handlers.onGoalNote?.('working')

    store.cancel()

    expect(streams[0].signal?.aborted).toBe(true)
    for (const item of store.activeItems.filter((item) => item.kind === 'reasoning' || item.kind === 'goal_note')) {
      expect(item.streaming).toBe(false)
    }
    await sending
    await vi.waitFor(() => expect(store.activeStatus).toBe('done'))
    expect(store.activeError).toBe('')
    expect(api.cancelRun).toHaveBeenCalledWith('run-1')
  })
})

describe('mid-turn steering (Codex parity)', () => {
  let streams: AttachedStream[]

  beforeEach(() => {
    streams = []
    let runCount = 0
    api.createRun.mockReset().mockImplementation(() => {
      runCount += 1
      return Promise.resolve({ run_id: `run-${runCount}`, state: 'running', queue_position: 0 })
    })
    api.cancelRun.mockReset().mockResolvedValue(undefined)
    api.queueRunInput.mockReset().mockResolvedValue({ ok: true, queued: 1 })
    api.streamRun.mockReset().mockImplementation((_runID, handlers, signal?: AbortSignal) => {
      streams.push({ signal, handlers })
      return new Promise<void>((resolve) => {
        signal?.addEventListener('abort', () => resolve(), { once: true })
      })
    })
  })

  it('steer() queues text into the live run without waiting for it to finish', async () => {
    const store = seedStore()
    const sending = store.send('first')
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    streams[0].handlers.onRunStarted?.()

    store.steer('actually focus on the failing test')

    expect(api.queueRunInput).toHaveBeenCalledWith('run-1', 'actually focus on the failing test')
    expect(store.activePendingSteers).toEqual(['actually focus on the failing test'])
    // Steering must not start a second run.
    expect(api.createRun).toHaveBeenCalledTimes(1)

    store.cancel()
    await sending
  })

  it('cancel() resubmits pending steers as a brand-new turn', async () => {
    const store = seedStore()
    const sending = store.send('first')
    await vi.waitFor(() => expect(streams).toHaveLength(1))
    streams[0].handlers.onRunStarted?.()

    store.steer('use this steering input')
    store.cancel()
    await sending

    await vi.waitFor(() => expect(streams).toHaveLength(2))
    expect(api.createRun).toHaveBeenCalledTimes(2)
    expect(api.createRun.mock.calls[1][1]).toBe('use this steering input')
    expect(store.activePendingSteers).toEqual([])
  })
})
