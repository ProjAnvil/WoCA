// Shared request core for every API module: the JSON envelope helpers and
// the streaming-transport overrides (native client SSE bypasses the Wails
// asset proxy and hits the bridge directly).
import i18n from '../i18n'

// Streaming base: inside the native client, SSE must bypass the Wails
// asset proxy (WebView2 buffers proxied streams) and hit the bridge
// directly; browsers keep same-origin relative paths ('').
let streamBase = ''
let streamBridge = ''
let streamAuth: { scheme: string; token: string } | null = null

/** setStreamBase points streaming requests at an absolute origin. */
export function setStreamBase(base: string) {
  streamBase = base ? base.replace(/\/$/, '') : ''
}

/**
 * setStreamBridge pins the target bridge for streaming requests aimed at
 * an edge access point (v1.6 入口②): native SSE bypasses the transport
 * proxy, so the routing header rides along explicitly.
 */
export function setStreamBridge(bridgeId: string) {
  streamBridge = bridgeId
}

/**
 * setStreamAuth carries the session material onto direct stream
 * connections: the native WebView's SSE bypasses the asset proxy (and
 * with it the proxy's Authorization injection), so remote profiles need
 * the Bearer here or the event stream answers 401.
 */
export function setStreamAuth(scheme: string, token: string) {
  streamAuth = scheme && token ? { scheme, token } : null
}

/** streamTarget snapshots the overrides a stream connection should use. */
export function streamTarget(): {
  base: string
  bridge: string
  auth: { scheme: string; token: string } | null
} {
  return { base: streamBase, bridge: streamBridge, auth: streamAuth }
}

/** Raised on 401 so the app can raise the bridge login gate. */
export class UnauthorizedError extends Error {
  /** Enumerated failure code from the 401 body (backend authn codes):
   * "unauthorized" | "revoked". */
  code: string

  constructor(code = 'unauthorized') {
    super('unauthorized')
    this.name = 'UnauthorizedError'
    this.code = code
  }
}

// Shared skeleton for every non-SSE endpoint: on a non-2xx it prefers the
// JSON body's {error} detail (the Go server's writeError always sends one)
// over the bare status code, prefixed with a human-readable label so callers
// never surface a context-free number. Success bodies are always JSON; an
// empty body decodes to undefined for the void endpoints.
export async function request<T>(url: string, init?: RequestInit, label?: string): Promise<T> {
  const resp = await fetch(url, init)
  if (resp.status === 401) {
    const code = await resp
      .json()
      .then((d) => (d as { code?: string }).code ?? 'unauthorized')
      .catch(() => 'unauthorized')
    throw new UnauthorizedError(code)
  }
  if (!resp.ok) {
    const detail = await resp
      .json()
      .then((d) => (d as { error?: string }).error)
      .catch(() => '')
    const msg = detail || String(resp.status)
    throw new Error(label ? `${label}：${msg}` : msg)
  }
  const text = await resp.text()
  return (text ? JSON.parse(text) : undefined) as T
}

export async function postJSON<T>(
  url: string,
  body: unknown,
  label = i18n.global.t('api.requestFailed'),
): Promise<T> {
  return request<T>(
    url,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
    label,
  )
}
