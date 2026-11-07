// init() boot paths: the global /api/events stream must be connected on
// every one — a fresh install (empty session list) and a failed listing
// still need activity_changed / wake / session-state signals. Regression
// test: the empty-list early return used to skip connectGlobalEvents, so a
// brand-new install never received activity updates until a reload.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  listSessions: vi.fn(),
  createSession: vi.fn(),
  streamEvents: vi.fn(),
  streamRun: vi.fn(),
  getSession: vi.fn(),
  getGoal: vi.fn(),
  getSessionContext: vi.fn(),
}))
vi.mock('../api/client', () => ({ ...api }))

import { useSessionStore } from './sessions'

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  api.streamEvents.mockReturnValue({ close: vi.fn() })
  api.getGoal.mockResolvedValue({ goal: null, running: false })
  api.getSession.mockResolvedValue({ messages: [] })
  api.getSessionContext.mockResolvedValue({ enabled: false, items: [], todos: [] })
})

describe('init global-stream wiring', () => {
  it('connects the global events stream on a fresh install (empty list)', async () => {
    api.listSessions.mockResolvedValue([])
    api.createSession.mockResolvedValue({ session_id: 's-new', workspace: '', workspace_name: '' })
    const store = useSessionStore()
    await store.init()
    expect(api.streamEvents).toHaveBeenCalledTimes(1)
    expect(store.sessions.map((s) => s.id)).toEqual(['s-new'])
  })

  it('connects the global events stream when the listing fails', async () => {
    api.listSessions.mockRejectedValue(new Error('listing down'))
    api.createSession.mockResolvedValue({ session_id: 's-err', workspace: '', workspace_name: '' })
    const store = useSessionStore()
    await store.init()
    expect(api.streamEvents).toHaveBeenCalledTimes(1)
  })

  it('connects exactly once when sessions exist', async () => {
    api.listSessions.mockResolvedValue([
      { id: 's1', title: 'S', updated_at: '', workspace: '', workspace_name: '' },
    ])
    const store = useSessionStore()
    await store.init()
    expect(api.streamEvents).toHaveBeenCalledTimes(1)
    expect(store.activeId).toBe('s1')
  })
})
