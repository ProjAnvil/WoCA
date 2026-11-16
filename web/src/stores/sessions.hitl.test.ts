// Task 11: the interrupt state as an ordered per-ID card map. A multi-card
// pause parks every pending card; SSE replay (refresh reattach) must dedupe
// by answer key instead of stacking duplicates; the legacy single-card
// events keep the flat Interrupt shape for InterruptModal.
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

// Starts a live run (the send → createRun → attachStream path) and returns
// its SSE handler table once the stream is attached.
async function startRun() {
  const handlers: Handlers = {}
  api.createRun.mockReset().mockResolvedValue({ run_id: 'run-1', state: 'running', queue_position: 0 })
  api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
  api.getSessionContext.mockReset().mockResolvedValue({})
  api.streamRun.mockReset().mockImplementation((_rid: string, h: Handlers) => {
    Object.assign(handlers, h)
    return new Promise<void>(() => {})
  })
  const store = seedStore()
  void store.send('go')
  await vi.waitFor(() => expect(handlers.onInterrupt).toBeTruthy())
  return { store, handlers }
}

const multiEvent = {
  kind: 'multi',
  payload: undefined,
  cards: [
    {
      id: 'call-main-1',
      ns: 'approval:se1',
      kind: 'permission',
      payload: {
        tool_name: 'Bash',
        tool_args: { command: 'git push' },
        reason: 'run',
        is_read_only: false,
        asks: [
          {
            call_id: 'call-main-1',
            tool_name: 'Bash',
            tool_args: { command: 'git push' },
            reason: 'run',
            origin: 'main',
            session_rule_key: 'bash:git push*',
          },
        ],
      },
    },
    {
      id: 'call-child-9',
      ns: 'subagent:researcher-ad33:se2',
      kind: 'permission',
      payload: {
        subagent: 'researcher',
        tool_name: 'WebFetch',
        tool_args: { url: 'https://example.com/x' },
        reason: 'fetch docs',
        is_read_only: true,
      },
    },
  ],
}

describe('multi-card interrupt parking', () => {
  it('parks every card of the pause in order and hides the legacy modal slot', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)
    expect(store.activeStatus).toBe('awaiting')
    expect(store.activeInterrupt).toBeNull() // InterruptModal stays hidden
    const cards = store.activeHitlCards!
    expect(cards).toHaveLength(2)
    expect(cards[0]).toMatchObject({ ns: 'approval:se1', kind: 'permission' })
    expect(cards[1]).toMatchObject({ ns: 'subagent:researcher-ad33:se2' })
  })

  it('replay of the same pause (refresh reattach) dedupes by answer key, keeping order', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)
    // The reattach path replays the same events from history.
    handlers.onInterrupt(multiEvent)
    handlers.onInterrupt(multiEvent)
    expect(store.activeHitlCards).toHaveLength(2)
    expect(store.activeHitlCards!.map((c) => c.ns)).toEqual(['approval:se1', 'subagent:researcher-ad33:se2'])
  })

  it('a re-parked pause after a partial answer replaces the map (resolved clears first)', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)
    handlers.onInterruptResolved()
    expect(store.activeHitlCards).toBeNull()

    // The server re-parks only the unanswered card under a new pause.
    handlers.onInterrupt({
      kind: 'multi',
      payload: undefined,
      cards: [multiEvent.cards[1]],
    })
    expect(store.activeHitlCards).toHaveLength(1)
    expect(store.activeHitlCards![0].ns).toBe('subagent:researcher-ad33:se2')
  })

  it('a single approvals-kind card parks the stack even without siblings', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt({
      kind: 'approvals',
      payload: { asks: [{ call_id: 'c1', tool_name: 'Bash', origin: 'main' }] },
      cards: [
        { id: 'c1', ns: 'approval:se1', kind: 'approvals', payload: { asks: [{ call_id: 'c1', tool_name: 'Bash', origin: 'main' }] } },
      ],
    })
    expect(store.activeInterrupt).toBeNull()
    expect(store.activeHitlCards).toHaveLength(1)
  })

  it('marks pending cards timed out when the parked TTL fires', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)
    handlers.onTimeout?.({ kind: 'multi', reason: 'parked_ttl' })
    expect(store.activeHitlCards!.every((c) => c.timedOut)).toBe(true)
  })
})

describe('legacy single-card interrupts stay compatible', () => {
  it('a flat event (no cards) keeps the activeInterrupt shape', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt({
      kind: 'permission',
      payload: { tool_name: 'Bash', tool_args: {}, reason: 'run', is_read_only: false },
    })
    expect(store.activeInterrupt).toMatchObject({ kind: 'permission' })
    expect(store.activeHitlCards).toBeNull()
    // The legacy scalar answer body is preserved.
    void store.answerInterrupt(true)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', { approve: true })
  })

  it('triggerInterrupt (demo path) still parks the legacy slot', () => {
    const store = seedStore()
    store.triggerInterrupt({ kind: 'ask', payload: { questions: [] } })
    expect(store.activeInterrupt).toMatchObject({ kind: 'ask' })
  })
})

describe('answering cards', () => {
  beforeEach(() => {
    api.answerRun.mockReset().mockResolvedValue(undefined)
    api.getGoal.mockReset().mockResolvedValue({ goal: null, running: false })
  })

  it('posts the NS-keyed answers list and keeps unanswered cards parked', async () => {
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)

    await store.answerCards([
      { key: 'approval:se1', value: { decisions: { 'call-main-1': { outcome: 'allow', grant: 'session' } } } },
    ])
    expect(api.answerRun).toHaveBeenCalledWith('run-1', {
      answers: [{ key: 'approval:se1', value: { decisions: { 'call-main-1': { outcome: 'allow', grant: 'session' } } } }],
    })
    // Partial answer: the sibling card stays parked, the session stays awaiting.
    expect(store.activeHitlCards).toHaveLength(1)
    expect(store.activeStatus).toBe('awaiting')

    await store.answerCards([{ key: 'subagent:researcher-ad33:se2', value: { approve: false } }])
    expect(store.activeHitlCards).toBeNull()
    expect(store.activeStatus).toBe('running')
  })

  it('a failed answer keeps the cards parked for retry', async () => {
    api.answerRun.mockRejectedValueOnce(new Error('boom'))
    const { store, handlers } = await startRun()
    handlers.onInterrupt(multiEvent)
    await expect(store.answerCards([{ key: 'approval:se1', value: { approve: true } }])).rejects.toThrow('boom')
    expect(store.activeHitlCards).toHaveLength(2)
  })
})
