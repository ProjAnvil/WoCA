import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { fetchEventSource } = vi.hoisted(() => ({ fetchEventSource: vi.fn() }))
vi.mock('@microsoft/fetch-event-source', () => ({ fetchEventSource }))

import { streamEvents, streamRun, getEdgeBridges, type StreamHandlers } from './client'
import { nativeRequestIDHeader } from '../native/requestCancellation'

function bridge(cancelRequest: NativeAppBridge['CancelRequest']): NativeAppBridge {
  return {
    Bootstrap: vi.fn<NativeAppBridge['Bootstrap']>().mockResolvedValue({ state: 'ready' }),
    Connect: vi.fn<NativeAppBridge['Connect']>().mockResolvedValue({ state: 'ready' }),
    Logout: vi.fn<NativeAppBridge['Logout']>().mockResolvedValue({ state: 'authorization_required' }),
    ConnectEmbedded: vi.fn<NativeAppBridge['ConnectEmbedded']>().mockResolvedValue({ state: 'ready' }),
    EdgeBridges: vi.fn<NativeAppBridge['EdgeBridges']>().mockResolvedValue([]),
    StreamCredentials: vi.fn<NativeAppBridge['StreamCredentials']>().mockResolvedValue({ scheme: '', token: '' }),
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
    await expect(streaming).rejects.toMatchObject({ name: 'AbortError' })

    expect(cancelRequest).toHaveBeenCalledWith(id)
  })

  it('rejects a pre-aborted stream without starting fetch or native registration', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: bridge(cancelRequest) } })
    const controller = new AbortController()
    controller.abort()

    await expect(streamRun('run-pre-aborted', handlers(), controller.signal)).rejects.toMatchObject({
      name: 'AbortError',
    })

    expect(fetchEventSource).not.toHaveBeenCalled()
    expect(cancelRequest).not.toHaveBeenCalled()
  })

  it('lets a same-tick abort win over an already-fulfilled transport promise', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: bridge(cancelRequest) } })
    fetchEventSource.mockResolvedValue(undefined)
    const controller = new AbortController()

    const streaming = streamRun('run-race', handlers(), controller.signal)
    controller.abort()

    await expect(streaming).rejects.toMatchObject({ name: 'AbortError' })
    expect(cancelRequest).toHaveBeenCalledOnce()
  })

  it('removes native abort listeners after a stream settles naturally', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: bridge(cancelRequest) } })
    fetchEventSource.mockResolvedValue(undefined)
    const controller = new AbortController()

    await streamRun('run-complete', handlers(), controller.signal)
    controller.abort()
    await Promise.resolve()

    expect(cancelRequest).not.toHaveBeenCalled()
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

describe('edge inventory probe', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function jsonResponse(body: unknown, status = 200) {
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  }

  it('returns the inventory when the origin is an edge access point', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse([
          { bridge_id: 'alpha', created_at: 1, last_seen: 5, live: true },
          { bridge_id: 'beta', created_at: 2, last_seen: 9, live: false },
        ]),
      ),
    )
    const bridges = await getEdgeBridges()
    expect(bridges).toEqual([
      { bridge_id: 'alpha', created_at: 1, last_seen: 5, live: true },
      { bridge_id: 'beta', created_at: 2, last_seen: 9, live: false },
    ])
    expect(fetch).toHaveBeenCalledWith('/api/edge/bridges', {
      headers: { Accept: 'application/json' },
    })
  })

  it('rejects the SPA fallback: 200 HTML is not an inventory', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response('<!doctype html><html></html>', { headers: { 'Content-Type': 'text/html' } })),
    )
    expect(await getEdgeBridges()).toBeNull()
  })

  it('returns null on non-2xx, bad shapes, and network errors', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: 'core plane unreachable' }, 502)))
    expect(await getEdgeBridges()).toBeNull()

    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ bridges: [] })))
    expect(await getEdgeBridges()).toBeNull()

    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('network')))
    expect(await getEdgeBridges()).toBeNull()
  })

  it('drops rows without a bridge id instead of failing the probe', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse([{ bridge_id: 'alpha', live: true }, { live: true }])),
    )
    expect(await getEdgeBridges()).toEqual([
      { bridge_id: 'alpha', created_at: 0, last_seen: 0, live: true },
    ])
  })
})

describe('request error envelope', () => {
  beforeEach(() => {
    vi.stubGlobal('go', undefined)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('raises UnauthorizedError with the enumerated code on 401', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'unauthorized', code: 'revoked' }), { status: 401 }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const { listSessions } = await import('./client')
    await expect(listSessions()).rejects.toMatchObject({ name: 'UnauthorizedError', code: 'revoked' })
  })

  it('labels non-2xx failures with the JSON error detail', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'forbidden' }), { status: 403 }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const { getSettings } = await import('./client')
    await expect(getSettings()).rejects.toThrow(/forbidden/)
  })

  it('falls back to the bare status code when the body is not JSON', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('nope', { status: 502 }))
    vi.stubGlobal('fetch', fetchMock)
    const { getSettings } = await import('./client')
    await expect(getSettings()).rejects.toThrow(/502/)
  })
})
