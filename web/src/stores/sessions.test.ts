import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  createRun: vi.fn(),
  streamRun: vi.fn(),
  cancelRun: vi.fn(),
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
    api.streamRun.mockReset().mockImplementation((_runID, handlers, signal?: AbortSignal) => {
      streams.push({ signal, handlers })
      return new Promise<void>((resolve, reject) => {
        signal?.addEventListener(
          'abort',
          () => reject(new DOMException('deliberate stream stop', 'AbortError')),
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
    await sending
    await vi.waitFor(() => expect(store.activeStatus).toBe('done'))
    expect(store.activeError).toBe('')
    expect(api.cancelRun).toHaveBeenCalledWith('run-1')
    for (const item of store.activeItems.filter((item) => item.kind === 'reasoning' || item.kind === 'goal_note')) {
      expect(item.streaming).toBe(false)
    }
  })

  it('treats a cross-realm-shaped AbortError as deliberate cleanup', async () => {
    api.streamRun.mockImplementation((_runID, handlers, signal?: AbortSignal) => {
      streams.push({ signal, handlers })
      return new Promise<void>((_resolve, reject) => {
        signal?.addEventListener('abort', () => {
          const error = new Error('foreign abort')
          error.name = 'AbortError'
          reject(error)
        }, { once: true })
      })
    })
    const store = seedStore()
    const sending = store.send('cancel cross realm')
    await vi.waitFor(() => expect(streams).toHaveLength(1))

    store.cancel()
    await sending
    await vi.waitFor(() => expect(store.activeStatus).toBe('done'))

    expect(store.activeError).toBe('')
  })
})
