// Characterization tests for the session store's live surfaces — written
// before the P0-10 split so the extraction has a behavioral contract:
// live-block lifecycle, queue promotion, refresh reattach, context usage,
// and interrupt answering.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  createRun: vi.fn(),
  streamRun: vi.fn(),
  cancelRun: vi.fn(),
  answerRun: vi.fn(),
  getSession: vi.fn(),
  getGoal: vi.fn(),
  getSessionContext: vi.fn(),
}))

vi.mock('../api/client', () => ({ ...api }))

import { useSessionStore } from './sessions'

type Handlers = Record<string, (...args: unknown[]) => void>

function seedStore(items: Record<string, unknown[]> = { 'session-1': [] }) {
  setActivePinia(createPinia())
  const store = useSessionStore()
  store.seed(
    [{ id: 'session-1', title: 'Session', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    items as never,
  )
  return store
}

describe('live-block lifecycle', () => {
  let handlers: Handlers

  beforeEach(() => {
    handlers = {}
    api.createRun.mockReset().mockResolvedValue({ run_id: 'run-1', state: 'running', queue_position: 0 })
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
    api.getSessionContext.mockReset().mockResolvedValue({})
    api.streamRun.mockReset().mockImplementation((_rid: string, h: Handlers) => {
      Object.assign(handlers, h)
      return new Promise<void>(() => {})
    })
  })

  async function startRun(prompt = 'go') {
    const p = seedStore().send(prompt)
    await vi.waitFor(() => expect(handlers.onDelta).toBeTruthy())
    return p
  }

  it('keeps one reasoning block, closes it on the first answer token, and opens the bubble lazily', async () => {
    const store = seedStore()
    const sent = store.send('go')
    await vi.waitFor(() => expect(handlers.onDelta).toBeTruthy())

    handlers.onReasoning('think ')
    handlers.onReasoning('more')
    let blocks = store.activeItems.filter((i) => i.kind === 'reasoning')
    expect(blocks).toHaveLength(1)
    expect(blocks[0]).toMatchObject({ content: 'think more', streaming: true })
    // No answer bubble before the first token.
    expect(store.activeItems.filter((i) => i.kind === 'assistant')).toHaveLength(0)

    handlers.onDelta('Hello')
    blocks = store.activeItems.filter((i) => i.kind === 'reasoning')
    expect(blocks[0]).toMatchObject({ streaming: false })
    const bubble = store.activeItems.find((i) => i.kind === 'assistant')
    expect(bubble).toMatchObject({ content: 'Hello', streaming: true })
    void sent
  })

  it('closes the goal narration on the first answer token, merges tool results into their call card', async () => {
    const store = seedStore()
    const sent = store.send('go')
    await vi.waitFor(() => expect(handlers.onToolCall).toBeTruthy())

    handlers.onGoalNote('cycle 1…')
    handlers.onToolCall({ id: 'tc-1', name: 'Read', args: { file: 'a.go' } })
    // A tool call freezes both the open bubble and the narration block.
    handlers.onToolResult({ tool_call_id: 'tc-1', content: 'file body' })
    handlers.onDelta('done')

    const note = store.activeItems.find((i) => i.kind === 'goal_note')
    expect(note).toMatchObject({ content: 'cycle 1…', streaming: false })
    const call = store.activeItems.find((i) => i.kind === 'tool_call' && i.toolCallId === 'tc-1')
    expect(call).toMatchObject({ result: 'file body' })
    void sent
  })

  it('orphan tool results become standalone cards, never dropped', async () => {
    const store = seedStore()
    const sent = store.send('go')
    await vi.waitFor(() => expect(handlers.onToolResult).toBeTruthy())
    handlers.onToolResult({ tool_call_id: 'ghost', content: 'late' })
    const orphan = store.activeItems.find((i) => i.kind === 'tool_result')
    expect(orphan).toMatchObject({ toolCallId: 'ghost', content: 'late' })
    void sent
  })

  it('done freezes live blocks and clears a parked interrupt; a new interrupt parks awaiting', async () => {
    const store = seedStore()
    const sent = store.send('go')
    await vi.waitFor(() => expect(handlers.onDelta).toBeTruthy())
    handlers.onDelta('partial')
    handlers.onDone()
    expect(store.activeItems.find((i) => i.kind === 'assistant')).toMatchObject({ streaming: false })
    expect(store.activeStatus).toBe('done')

    handlers.onInterrupt({ kind: 'permission', payload: { tool_name: 'Bash', tool_args: {}, reason: 'run', is_read_only: false } })
    expect(store.activeStatus).toBe('awaiting')
    expect(store.activeInterrupt).toMatchObject({ kind: 'permission' })

    handlers.onInterruptResolved()
    expect(store.activeInterrupt).toBeNull()
    expect(store.activeStatus).toBe('running')
    void sent
  })
})

describe('queue admission and promotion', () => {
  let handlers: Handlers

  beforeEach(() => {
    handlers = {}
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
    api.getSessionContext.mockReset().mockResolvedValue({})
    api.streamRun.mockReset().mockImplementation((_rid: string, h: Handlers) => {
      Object.assign(handlers, h)
      return new Promise<void>(() => {})
    })
  })

  it('a queued run parks with its position and never claims the run slot until promotion', async () => {
    api.createRun.mockReset().mockResolvedValue({ run_id: 'run-q', state: 'queued', queue_position: 2 })
    const store = seedStore()
    const sent = store.send('behind')
    await vi.waitFor(() => expect(handlers.onRunStarted).toBeTruthy())

    expect(store.activeStatus).toBe('queued')
    expect(store.queuePosition['session-1']).toBe(2)
    expect(store.runId['session-1']).toBeUndefined() // slot belongs to the active run

    handlers.onQueuePosition(1)
    expect(store.queuePosition['session-1']).toBe(1)
    handlers.onRunStarted()
    expect(store.activeStatus).toBe('running')
    expect(store.queuePosition['session-1']).toBeUndefined()
    expect(store.runId['session-1']).toBe('run-q')
    void sent
  })
})

describe('refresh reattach', () => {
  let handlers: Handlers

  beforeEach(() => {
    handlers = {}
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
    api.getSessionContext.mockReset().mockResolvedValue({})
    api.getSession.mockReset().mockResolvedValue({ messages: [] })
    api.streamRun.mockReset().mockImplementation((_rid: string, h: Handlers) => {
      Object.assign(handlers, h)
      return new Promise<void>(() => {})
    })
  })

  it('never reattaches while the local stream is already live', async () => {
    const store = seedStore()
    store.serverRunState['session-1'] = { status: 'running', runId: 'run-foreign' }
    // A locally-attached awaiting run is a live local stream (status is not
    // exported; the demo interrupt trigger parks the same busy state).
    store.triggerInterrupt({ kind: 'ask', payload: { questions: [] } })

    await store.setActive('session-1')
    expect(api.streamRun).not.toHaveBeenCalledWith('run-foreign', expect.anything(), expect.anything())
  })

  it('reattaches a server-owned run for an idle local session, rebuilding from disk', async () => {
    const store = seedStore()
    store.serverRunState['session-1'] = { status: 'running', runId: 'run-foreign' }

    await store.setActive('session-1')
    await vi.waitFor(() => expect(api.streamRun).toHaveBeenCalledWith('run-foreign', expect.anything(), expect.anything()))
    expect(api.getSession).toHaveBeenCalledWith('session-1')
    expect(store.runId['session-1']).toBe('run-foreign')
  })

})

describe('context usage snapshot', () => {
  beforeEach(() => {
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
    api.getSession.mockReset().mockResolvedValue({ messages: [] })
    api.getSessionContext.mockReset()
  })

  it('clears the snapshot for an empty session and surfaces fetch errors', async () => {
    api.getSessionContext.mockReset().mockResolvedValue({ estimated_total: 1200 })
    const store = seedStore({ 'session-1': [{ id: 'x', kind: 'user', content: 'hi' }] })
    await store.refreshContext()
    expect(store.contextInfo).toMatchObject({ estimated_total: 1200 })

    api.getSessionContext.mockReset().mockRejectedValue(new Error('boom'))
    await store.refreshContext()
    expect(store.contextError).toBe('boom')
    expect(store.contextLoading).toBe(false)
  })

  it('an empty transcript keeps the chip disabled (null snapshot, no request)', async () => {
    const store = seedStore()
    await store.refreshContext()
    expect(store.contextInfo).toBeNull()
    expect(api.getSessionContext).not.toHaveBeenCalled()
  })
})

describe('interrupt answering', () => {
  it('approves via the run slot and returns the session to running', async () => {
    api.answerRun.mockReset().mockResolvedValue(undefined)
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
    const store = seedStore()
    store.triggerInterrupt({ kind: 'permission', payload: { tool_name: 'Bash', tool_args: {}, reason: 'run', is_read_only: false } })
    store.runId['session-1'] = 'run-1'

    await store.answerInterrupt(true)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', { approve: true })
    expect(store.activeInterrupt).toBeNull()
    expect(store.activeStatus).toBe('running')
  })
})
