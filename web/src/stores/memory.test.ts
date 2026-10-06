// The memory panel store: open/reload lifecycle, browse-mode paging with
// batch-size exhaustion, the remember gate notice ordering, and search's
// debounce + cancel-on-close.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  listMemory: vi.fn(),
  memoryStats: vi.fn(),
  rememberMemory: vi.fn(),
  forgetMemory: vi.fn(),
}))
vi.mock('../api/client', () => ({ ...api }))

import { useMemoryStore } from './memory'

const item = (id: string) => ({ id, content: `c-${id}`, layer: 'facts', tags: [], updated_at: '' })
const page = (n: number, from = 0) => Array.from({ length: n }, (_, i) => item(`i${from + i}`))

function resolveList(items: ReturnType<typeof page>, engine = 'ladym') {
  api.listMemory.mockResolvedValueOnce({ engine, items })
}
const flushMicrotasks = () => Promise.resolve().then(() => Promise.resolve())

beforeEach(() => {
  vi.useFakeTimers()
  setActivePinia(createPinia())
  vi.clearAllMocks()
  api.memoryStats.mockResolvedValue({ stats: { total: 0, layers: {} } })
})

describe('memory store', () => {
  it('open() resets filters, loads the first page and the stats', async () => {
    resolveList(page(20))
    api.memoryStats.mockResolvedValueOnce({ stats: { total: 42, layers: { facts: 42 } } })
    const m = useMemoryStore()
    m.query = 'stale'
    m.open('/ws', 'ws-name')
    expect(m.panelOpen).toBe(true)
    expect(m.query).toBe('')
    await flushMicrotasks()
    expect(api.listMemory).toHaveBeenCalledWith({
      workspace: '/ws',
      q: undefined,
      layer: undefined,
      limit: 20,
    })
    expect(m.items).toHaveLength(20)
    expect(m.stats).toEqual({ total: 42, layers: { facts: 42 } })
    expect(m.loading).toBe(false)
    expect(m.engine).toBe('ladym')
  })

  it('reload failures land on the error channel', async () => {
    api.listMemory.mockRejectedValueOnce(new Error('core down'))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()
    expect(m.error).toContain('core down')
    expect(m.loading).toBe(false)
  })

  it('loadMore appends the next page and reports exhaustion from batch size', async () => {
    resolveList(page(20))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()
    expect(m.hasMore()).toBe(true) // full page → maybe more

    resolveList(page(5, 20))
    await m.loadMore()
    expect(api.listMemory).toHaveBeenLastCalledWith(
      expect.objectContaining({ offset: 20, limit: 20 }),
    )
    expect(m.items).toHaveLength(25)
    expect(m.hasMore()).toBe(false) // short batch → end of listing
  })

  it('search mode disables load-more and reload is debounced', async () => {
    resolveList(page(20))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()

    m.query = 'needle'
    m.searchDebounced()
    m.searchDebounced() // second keystroke restarts the timer
    expect(api.listMemory).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(300)
    await flushMicrotasks()
    expect(api.listMemory).toHaveBeenCalledTimes(2)
    expect(m.hasMore()).toBe(false) // recall ignores offset in search mode
  })

  it('close() cancels a queued debounced search', async () => {
    resolveList(page(1))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()
    m.query = 'x'
    m.searchDebounced()
    m.close()
    vi.advanceTimersByTime(300)
    await flushMicrotasks()
    expect(api.listMemory).toHaveBeenCalledTimes(1) // the queued one never fired
  })

  it('add() writes the gate-drop notice AFTER the reload that clears it', async () => {
    resolveList(page(1))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()

    api.rememberMemory.mockResolvedValueOnce({ id: '', dropped: true } as never)
    resolveList(page(1))
    await m.add('fact')
    expect(m.notice).not.toBe('') // droppedByGate survived the reload

    api.rememberMemory.mockResolvedValueOnce({ id: 'mem-1', dropped: false } as never)
    resolveList(page(1))
    const id = await m.add('fact 2')
    expect(id).toBe('mem-1')
    expect(m.notice).toBe('')
  })

  it('add() failures return an empty id with a localized message', async () => {
    resolveList(page(1))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()
    api.rememberMemory.mockRejectedValueOnce(new Error('gate closed'))
    await expect(m.add('fact')).resolves.toBe('')
    expect(m.error).toContain('gate closed')
  })

  it('remove() splices locally, refreshes, and keeps failures visible', async () => {
    resolveList(page(2))
    const m = useMemoryStore()
    m.open('/ws')
    await flushMicrotasks()

    api.forgetMemory.mockResolvedValueOnce(undefined)
    resolveList(page(2)) // the refresh re-lists both rows (server is source of truth)
    await m.remove('i0')
    await flushMicrotasks()
    expect(api.forgetMemory).toHaveBeenCalledWith('i0')
    expect(m.items).toHaveLength(2)

    api.forgetMemory.mockRejectedValueOnce(new Error('readonly'))
    await m.remove('i1')
    expect(m.error).toContain('readonly')
  })
})
