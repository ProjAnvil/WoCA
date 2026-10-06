// Store action surfaces beyond the live-block harness (sessions.stream):
// steering, cancellation with pending steers, interrupt answering, agent
// commands, and goal commands.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  createRun: vi.fn(),
  streamRun: vi.fn(),
  cancelRun: vi.fn(),
  answerRun: vi.fn(),
  queueRunInput: vi.fn(),
  sendGoalCommand: vi.fn(),
  getSession: vi.fn(),
  getGoal: vi.fn().mockResolvedValue({ goal: null, running: false }),
  getSessionContext: vi.fn(),
  getAgent: vi.fn(),
  getQuota: vi.fn(),
  setPermissionMode: vi.fn(),
  setThinkingLevel: vi.fn(),
}))
vi.mock('../api/client', () => ({ ...api }))

import { useSessionStore } from './sessions'
import type { ChatItem } from '../types'

function storeWithRun() {
  const store = useSessionStore()
  store.seed(
    [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    { s1: [] },
  )
  store.status = { s1: 'running' }
  store.runId = { s1: 'run-1' }
  return store
}

const userItems = (store: ReturnType<typeof useSessionStore>) =>
  store.activeItems.filter((i) => i.kind === 'user') as Extract<ChatItem, { kind: 'user' }>[]
const lastItem = (store: ReturnType<typeof useSessionStore>) => store.activeItems.at(-1)!

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  // Promise-returning defaults so the store's .catch/.then chains hold.
  api.streamRun.mockResolvedValue(undefined)
  api.cancelRun.mockResolvedValue(undefined)
  api.answerRun.mockResolvedValue(undefined)
  api.queueRunInput.mockResolvedValue(undefined)
  api.setPermissionMode.mockResolvedValue({ ok: true, mode: 'default' })
  api.setThinkingLevel.mockResolvedValue({ ok: true, level: 'off', budget: 0 })
  api.createRun.mockResolvedValue({ run_id: 'run-1', state: 'running', queue_position: 0 })
  api.getGoal.mockResolvedValue({ goal: null, running: false })
})

describe('steer', () => {
  it('queues the text and forwards it to the live run input', () => {
    const store = storeWithRun()
    store.steer('  faster  ')
    expect(store.activePendingSteers).toEqual(['faster'])
    expect(api.queueRunInput).toHaveBeenCalledWith('run-1', 'faster')
  })

  it('is a silent no-op without a live run or text', () => {
    const store = storeWithRun()
    store.runId = { s1: '' }
    store.steer('faster')
    store.runId = { s1: 'run-1' }
    store.steer('   ')
    expect(api.queueRunInput).not.toHaveBeenCalled()
    expect(store.activePendingSteers).toEqual([])
  })

  it('surfaces queueing failures on the error channel', async () => {
    const store = storeWithRun()
    api.queueRunInput.mockRejectedValueOnce(new Error('nope'))
    store.steer('faster')
    await Promise.resolve()
    await Promise.resolve()
    expect(store.activeError).toContain('nope')
    // The pending steer stays visible (stop resends it as a fresh turn).
    expect(store.activePendingSteers).toEqual(['faster'])
  })
})

describe('cancel', () => {
  it('with a live run: closes blocks and waits for the server confirm', async () => {
    const store = storeWithRun()
    api.cancelRun.mockResolvedValueOnce(undefined)
    store.steer('after')
    store.cancel()
    expect(store.activeInterrupt).toBeNull()
    expect(store.activePendingSteers).toEqual([]) // taken for resubmission
    expect(store.activeStatus).toBe('running') // not done until the server confirms
    await Promise.resolve()
    await Promise.resolve()
    expect(store.activeStatus).toBe('done')
  })

  it('resubmits pending steers as one fresh turn after a confirmed cancel', async () => {
    const store = storeWithRun()
    api.cancelRun.mockResolvedValueOnce(undefined)
    api.createRun.mockResolvedValueOnce({ run_id: 'run-2', state: 'running', queue_position: 0 })
    store.steer('a')
    store.steer('b')
    store.cancel()
    await Promise.resolve()
    await Promise.resolve()
    await Promise.resolve()
    expect(api.createRun).toHaveBeenCalledWith('s1', 'a\nb', '', [], undefined)
    expect(userItems(store).at(-1)!.content).toBe('a\nb')
  })

  it('without a run: settles to done immediately, no server call', () => {
    const store = storeWithRun()
    store.runId = { s1: '' }
    store.status = { s1: 'awaiting' }
    store.cancel()
    expect(api.cancelRun).not.toHaveBeenCalled()
    expect(store.activeStatus).toBe('done')
  })

  it('a failed cancel keeps the failure visible instead of faking done', async () => {
    const store = storeWithRun()
    api.cancelRun.mockRejectedValueOnce(new Error('still live'))
    store.cancel()
    await Promise.resolve()
    await Promise.resolve()
    expect(store.activeStatus).toBe('error')
    expect(store.activeError).toContain('still live')
  })
})

describe('answerInterrupt', () => {
  it('clears demo interrupts locally as done', () => {
    const store = storeWithRun()
    store.triggerInterrupt({ kind: 'permission', payload: {} } as never)
    store.runId = { s1: '' }
    void store.answerInterrupt(true)
    expect(store.activeInterrupt).toBeNull()
    expect(store.activeStatus).toBe('done')
  })

  it('answers ask interrupts with the typed answers', async () => {
    const store = storeWithRun()
    store.triggerInterrupt({
      kind: 'ask',
      payload: { questions: [{ header: '', question: 'q', options: [] }] },
    } as never)
    api.answerRun.mockResolvedValueOnce(undefined)
    await store.answerInterrupt(true, ['x', 'y'])
    expect(api.answerRun).toHaveBeenCalledWith('run-1', { answers: ['x', 'y'] })
    expect(store.activeStatus).toBe('running')
    expect(store.activeInterrupt).toBeNull()
  })

  it('answers approve/deny interrupts and clears on failure', async () => {
    const store = storeWithRun()
    store.triggerInterrupt({ kind: 'plan', payload: { plan: '' } } as never)
    api.answerRun.mockResolvedValueOnce(undefined)
    await store.answerInterrupt(false)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', { approve: false })

    store.triggerInterrupt({ kind: 'permission', payload: {} } as never)
    api.answerRun.mockRejectedValueOnce(new Error('gone'))
    await store.answerInterrupt(true)
    expect(store.activeStatus).toBe('error')
    expect(store.activeInterrupt).toBeNull()
  })
})

describe('tryAgentCommand', () => {
  it('returns false for plain prompts and empty sessions', async () => {
    const store = storeWithRun()
    await store.send('/plain hello')
    expect(userItems(store).length).toBe(1) // fell through to a model turn
  })

  it('/permissions <mode> switches and answers in a bubble', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.createRun.mockResolvedValue({ run_id: 'run-x', state: 'running', queue_position: 0 })
    api.setPermissionMode.mockResolvedValueOnce({ ok: true, mode: 'plan' })
    await store.send('/permissions plan')
    expect(api.setPermissionMode).toHaveBeenCalledWith('plan')
    const bubble = lastItem(store) as { kind: string; content: string }
    expect(bubble.kind).toBe('assistant')
    // Mode label echoes through the localized command answer bubble.
    expect(bubble.content.length).toBeGreaterThan(0)
    expect(store.activeStatus).toBe('done')
  })

  it('/thinking <level> switches the thinking budget', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.createRun.mockResolvedValue({ run_id: 'run-x', state: 'running', queue_position: 0 })
    api.setThinkingLevel.mockResolvedValueOnce({ ok: true, level: 'high', budget: 10000 })
    await store.send('/thinking high')
    expect(api.setThinkingLevel).toHaveBeenCalledWith('high')
    expect(lastItem(store).kind).toBe('assistant')
  })

  it('failures leave a chat bubble, not a disappearing banner', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.createRun.mockResolvedValue({ run_id: 'run-x', state: 'running', queue_position: 0 })
    api.setPermissionMode.mockRejectedValueOnce(new Error('denied'))
    await store.send('/permissions plan')
    const bubble = lastItem(store) as { content: string }
    expect(bubble.content).toContain('denied')
    expect(store.activeStatus).toBe('done')
  })
})

describe('sendGoalCommand', () => {
  it('is a no-op without a session or while busy', async () => {
    const store = useSessionStore() // no active session
    await store.sendGoalCommand('/goal set x')
    expect(api.sendGoalCommand).not.toHaveBeenCalled()

    const busy = storeWithRun()
    await busy.sendGoalCommand('/goal set x')
    expect(api.sendGoalCommand).not.toHaveBeenCalled()
  })

  it('echoes the command, applies the goal and streams a started loop', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.createRun.mockResolvedValue({ run_id: 'run-x', state: 'running', queue_position: 0 })
    api.sendGoalCommand.mockResolvedValueOnce({
      ok: true,
      output: 'goal set',
      goal: { objective: 'x', status: 'running', cycles: 0, max_cycles: 5 } as never,
      run_id: 'run-loop',
    })
    await store.sendGoalCommand('set x')
    expect(api.sendGoalCommand).toHaveBeenCalledWith('s1', 'set x', '')
    expect(userItems(store).at(-1)!.content).toBe('/goal set x')
    expect(lastItem(store).kind).toBe('assistant')
    expect(store.activeGoal?.objective).toBe('x')
  })

  it('marks done when the command starts no loop', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.sendGoalCommand.mockResolvedValueOnce({ ok: true, output: 'cleared', goal: null, run_id: '' })
    await store.sendGoalCommand('clear')
    expect(store.activeStatus).toBe('done')
  })

  it('surfaces command failures', async () => {
    const store = storeWithRun()
    store.status = { s1: 'idle' }
    api.sendGoalCommand.mockRejectedValueOnce(new Error('bad goal'))
    await store.sendGoalCommand('set x')
    expect(store.activeStatus).toBe('error')
    expect(store.activeError).toContain('bad goal')
  })
})
