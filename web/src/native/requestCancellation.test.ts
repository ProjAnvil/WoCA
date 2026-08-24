import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { nativeRequestCancellation, nativeRequestIDHeader } from './requestCancellation'

function nativeBridge(cancelRequest: NativeAppBridge['CancelRequest']): NativeAppBridge {
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

describe('nativeRequestCancellation', () => {
  beforeEach(() => {
    vi.stubGlobal('go', undefined)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('leaves browser requests and aborts outside the Wails bridge', () => {
    const controller = new AbortController()
    const cancellation = nativeRequestCancellation(controller.signal, { 'Last-Event-ID': '7' })

    expect(cancellation.headers).toEqual({ 'Last-Event-ID': '7' })
    expect(cancellation.headers[nativeRequestIDHeader]).toBeUndefined()
    controller.abort()
    cancellation.release()
  })

  it('adds a strong opaque native ID and cancels that exact request once', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockResolvedValue(true)
    vi.stubGlobal('go', { main: { App: nativeBridge(cancelRequest) } })
    vi.stubGlobal('crypto', {
      getRandomValues(bytes: Uint8Array) {
        for (let index = 0; index < bytes.length; index++) bytes[index] = index
        return bytes
      },
    })
    const controller = new AbortController()
    const cancellation = nativeRequestCancellation(controller.signal, { 'Last-Event-ID': '9' })

    const id = cancellation.headers[nativeRequestIDHeader]
    expect(id).toMatch(/^[A-Za-z0-9_-]{43}$/)
    expect(cancellation.headers['Last-Event-ID']).toBe('9')
    controller.abort()
    controller.abort()
    await Promise.resolve()

    expect(cancelRequest).toHaveBeenCalledTimes(1)
    expect(cancelRequest).toHaveBeenCalledWith(id)
    cancellation.release()
  })

  it('cancels immediately when registration follows abort and consumes bridge rejection', async () => {
    const cancelRequest = vi.fn<NativeAppBridge['CancelRequest']>().mockRejectedValue(new Error('gone'))
    vi.stubGlobal('go', { main: { App: nativeBridge(cancelRequest) } })
    const controller = new AbortController()
    controller.abort()

    const cancellation = nativeRequestCancellation(controller.signal)
    await Promise.resolve()
    await Promise.resolve()

    expect(cancelRequest).toHaveBeenCalledOnce()
    expect(cancelRequest).toHaveBeenCalledWith(cancellation.headers[nativeRequestIDHeader])
    cancellation.release()
  })

  it('contains a synchronous bridge dispatch failure during abort', () => {
    const cancelRequest = vi.fn(() => {
      throw new Error('bridge unavailable')
    }) as NativeAppBridge['CancelRequest']
    vi.stubGlobal('go', { main: { App: nativeBridge(cancelRequest) } })
    const controller = new AbortController()
    nativeRequestCancellation(controller.signal)

    expect(() => controller.abort()).not.toThrow()
    expect(cancelRequest).toHaveBeenCalledOnce()
  })
})
