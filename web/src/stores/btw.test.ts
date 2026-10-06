// The /btw side channel: ephemeral entries, ask lifecycle (stream deltas
// appended in place), and guard rails.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  startBtwRun: vi.fn(),
  streamRun: vi.fn(),
}))
vi.mock('../api/client', () => ({ ...api }))

import { useBtwStore } from './btw'

type StreamHandlers = {
  onDelta: (t: string) => void
  onDone: () => void
  onError: (m: string) => void
}

function askWithStream(store: ReturnType<typeof useBtwStore>, prompt: string, drive?: (h: StreamHandlers) => void) {
  let handlers!: StreamHandlers
  api.streamRun.mockImplementationOnce(async (_id: string, h: StreamHandlers) => {
    handlers = h
    drive?.(h)
  })
  return { promise: store.ask(prompt, 's1', '/w'), handlers: () => handlers }
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  api.startBtwRun.mockResolvedValue({ run_id: 'run-btw' })
  api.streamRun.mockResolvedValue(undefined)
})

describe('btw side channel', () => {
  it('panel visibility helpers', () => {
    const btw = useBtwStore()
    btw.show()
    expect(btw.open).toBe(true)
    btw.hide()
    expect(btw.open).toBe(false)
    btw.entries = [{ role: 'user', content: 'x' }] as never
    btw.error = 'x'
    btw.clear()
    expect(btw.entries).toEqual([])
    expect(btw.error).toBe('')
  })

  it('ask appends the question, streams the answer in place, settles', async () => {
    const btw = useBtwStore()
    const { promise } = askWithStream(btw, '  quick one  ', (h) => {
      h.onDelta('an')
      h.onDelta('swer')
      h.onDone()
    })
    await promise
    expect(api.startBtwRun).toHaveBeenCalledWith('s1', 'quick one', '/w')
    expect(btw.entries).toEqual([
      { role: 'user', content: 'quick one' },
      { role: 'assistant', content: 'answer' },
    ])
    expect(btw.running).toBe(false)
    expect(btw.open).toBe(true)
    expect(btw.error).toBe('')
  })

  it('ignores empty asks, mid-flight asks, and missing sessions', async () => {
    const btw = useBtwStore()
    await btw.ask('   ', 's1')
    await btw.ask('q', '')
    expect(api.startBtwRun).not.toHaveBeenCalled()

    const inflight = askWithStream(btw, 'first')
    await btw.ask('second', 's1') // running: dropped
    expect(api.startBtwRun).toHaveBeenCalledTimes(1)
    inflight.handlers().onDone()
    await inflight.promise
  })

  it('stream errors surface and settle the channel; so do transport failures', async () => {
    const btw = useBtwStore()
    const bad = askWithStream(btw, 'q', (h) => h.onError('model down'))
    await bad.promise
    expect(btw.error).toBe('model down')
    expect(btw.running).toBe(false)

    api.startBtwRun.mockRejectedValueOnce(new Error('no side runs'))
    await btw.ask('again', 's1')
    expect(btw.error).toContain('no side runs')
    expect(btw.running).toBe(false)
  })
})
