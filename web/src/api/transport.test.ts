// The request core's success paths and helpers (error envelopes live in
// client.test.ts): JSON decoding, empty-body voids, postJSON's envelope,
// the error-code → message mapping, and the stream override snapshot.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiErrorText, ApiError, postJSON, request, setStreamAuth, setStreamBase, setStreamBridge, streamTarget, UnauthorizedError } from './transport'

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

afterEach(() => {
  vi.unstubAllGlobals()
  setStreamBase('')
  setStreamBridge('')
  setStreamAuth('', '')
})

describe('request core', () => {
  it('decodes JSON bodies and maps empty bodies to undefined (void endpoints)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ ok: true })))
    await expect(request('/api/x')).resolves.toEqual({ ok: true })

    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 204 })))
    await expect(request('/api/x')).resolves.toBeUndefined()
  })

  it('postJSON wraps method, content type and the default label', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({}))
    vi.stubGlobal('fetch', fetchMock)
    await postJSON('/api/x', { a: 1 })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/x')
    expect(init.method).toBe('POST')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ a: 1 })
  })
})

describe('error mapping helpers', () => {
  it('UnauthorizedError defaults to the unauthorized code', () => {
    expect(new UnauthorizedError().code).toBe('unauthorized')
    expect(new UnauthorizedError('revoked').code).toBe('revoked')
  })

  it('apiErrorText prefers the code translation, then the wire message, then the fallback', () => {
    // No registered translation for a made-up code → the message wins.
    expect(apiErrorText(new ApiError('wire detail', 'S-Z-9999'), 'fallback')).toBe('wire detail')
    // Without a code (legacy responses) → the caller's fallback.
    expect(apiErrorText(new ApiError('wire detail'), 'fallback')).toBe('fallback')
    // Non-ApiError throwables always fall back.
    expect(apiErrorText(new Error('network'), 'fallback')).toBe('fallback')
  })
})

describe('stream override snapshot', () => {
  it('setters normalize and clear', () => {
    setStreamBase('https://x.example.com/')
    setStreamBridge('br-1')
    setStreamAuth('Bearer', 'tok')
    expect(streamTarget()).toEqual({
      base: 'https://x.example.com',
      bridge: 'br-1',
      auth: { scheme: 'Bearer', token: 'tok' },
    })
    // An empty pair clears auth instead of sending an empty Authorization.
    setStreamAuth('', '')
    expect(streamTarget().auth).toBeNull()
  })
})
