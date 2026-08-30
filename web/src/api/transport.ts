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

// A 401 anywhere means the session is gone (expired, revoked, or never
// established): the login gate must come up on the FIRST failure instead
// of toasting "unauthorized" on every action while the app pretends to be
// signed in. The transport layer stays store-free; the app entry wires
// the handler (no import cycle).
let unauthorizedHandler: ((code: string) => void) | null = null

export function setUnauthorizedHandler(fn: ((code: string) => void) | null): void {
  unauthorizedHandler = fn
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
    unauthorizedHandler?.(code)
    throw new UnauthorizedError(code)
  }
  if (!resp.ok) {
    const body = await resp
      .json()
      .then((d) => d as { error?: string; code?: string })
      .catch(() => ({}) as { error?: string; code?: string })
    const msg = body.error || String(resp.status)
    throw new ApiError(label ? `${label}：${msg}` : msg, body.code)
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

/**
 * Standard error-code → localized message lookup (api-standard.md §1).
 * Codes are never localized server-side; the frontend owns the mapping.
 * Falls back to the English wire message when no translation exists.
 */
/** Generic non-2xx failure; carries the standard numeric code when the
 * server sent one (api-standard.md §1). */
export class ApiError extends Error {
  /** Standard numeric error code, e.g. "S-B-0100" (absent on legacy
   * responses that predate the registry). */
  code?: string

  constructor(message: string, code?: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
  }
}

export function apiErrorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.code) {
    const key = `errors.${err.code}`
    const t = (i18n.global as { t: (k: string) => string }).t
    const tr = t(key)
    if (tr !== key) return tr
    if (err.message) return err.message
  }
  return fallback
}
