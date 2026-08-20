import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { fetchEventSource } = vi.hoisted(() => ({ fetchEventSource: vi.fn() }))
vi.mock('@microsoft/fetch-event-source', () => ({ fetchEventSource }))

import { streamEvents, streamRun, type StreamHandlers } from './client'
import { nativeRequestIDHeader } from '../native/requestCancellation'

function bridge(cancelRequest: NativeAppBridge['CancelRequest']): NativeAppBridge {
  return {
    Bootstrap: vi.fn<NativeAppBridge['Bootstrap']>().mockResolvedValue({ state: 'ready' }),
    Connect: vi.fn<NativeAppBridge['Connect']>().mockResolvedValue({ state: 'ready' }),
    Logout: vi.fn<NativeAppBridge['Logout']>().mockResolvedValue({ state: 'authorization_required' }),
    Preference: vi.fn(async () => ({ value: '', found: false })),
    SetPreference: vi.fn(async (input) => ({ revision: input.revision, applied: true })),
    CancelRequest: cancelRequest,
  }
}

function handlers(): StreamHandlers {
  return {
    onDelta: vi.fn(),
    onToolCall: vi.fn(),
    onToolResult: vi.fn(),
    onInterrupt: vi.fn(),
    onDone: vi.fn(),
    onError: vi.fn(),
  }
}

describe('cancellable SSE transport', () => {
  beforeEach(() => {
    fetchEventSource.mockReset()
    vi.stubGlobal('go', undefined)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('adds a native request ID and forwards the caller AbortSignal to streamRun', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: bridge(cancelRequest) } })
    fetchEventSource.mockImplementation((_url, options) =>
      new Promise<void>((resolve) => options.signal?.addEventListener('abort', () => resolve(), { once: true })),
    )
    const controller = new AbortController()

    const streaming = streamRun('run-1', handlers(), controller.signal)
    const options = fetchEventSource.mock.calls[0][1]
    const id = options.headers[nativeRequestIDHeader]
    expect(id).toMatch(/^[A-Za-z0-9_-]{43}$/)
    expect(options.signal).toBe(controller.signal)
    controller.abort()
    await streaming

    expect(cancelRequest).toHaveBeenCalledWith(id)
  })

  it('keeps browser streamRun free of native headers and bridge calls', async () => {
    fetchEventSource.mockResolvedValue(undefined)
    const controller = new AbortController()

    await streamRun('run-browser', handlers(), controller.signal)

    const options = fetchEventSource.mock.calls[0][1]
    expect(options.signal).toBe(controller.signal)
    expect(options.headers[nativeRequestIDHeader]).toBeUndefined()
  })

  it('keeps browser global events on EventSource', () => {
    class BrowserEventSource {
      static instances: BrowserEventSource[] = []
      readonly url: string
      readonly listeners: Record<string, EventListener[]> = {}
      closed = false

      constructor(url: string) {
        this.url = url
        BrowserEventSource.instances.push(this)
      }

      addEventListener(type: string, listener: EventListener) {
        ;(this.listeners[type] ??= []).push(listener)
      }

      close() {
        this.closed = true
      }
    }
    vi.stubGlobal('EventSource', BrowserEventSource)

    const stream = streamEvents(vi.fn())

    expect(BrowserEventSource.instances).toHaveLength(1)
    expect(BrowserEventSource.instances[0].url).toBe('/api/events')
    expect(fetchEventSource).not.toHaveBeenCalled()
    stream.close()
    expect(BrowserEventSource.instances[0].closed).toBe(true)
  })

  it('uses cancellable fetch for native global events and close reaches Go', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: bridge(cancelRequest) } })
    fetchEventSource.mockImplementation((_url, options) =>
      new Promise<void>((resolve) => options.signal?.addEventListener('abort', () => resolve(), { once: true })),
    )

    const stream = streamEvents(vi.fn())
    const options = fetchEventSource.mock.calls[0][1]
    const id = options.headers[nativeRequestIDHeader]
    expect(() => options.onclose()).toThrow('global event stream closed')
    stream.close()
    await Promise.resolve()

    expect(options.signal.aborted).toBe(true)
    expect(() => options.onclose()).not.toThrow()
    expect(cancelRequest).toHaveBeenCalledWith(id)
  })

  it('consumes native global stream rejection without an unhandled promise', async () => {
    vi.stubGlobal('go', {
      main: { App: bridge(vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)) },
    })
    fetchEventSource.mockRejectedValue(new Error('connection failed'))

    const stream = streamEvents(vi.fn())
    await Promise.resolve()
    await Promise.resolve()
    stream.close()
  })
})
