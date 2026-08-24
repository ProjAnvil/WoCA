import { fetchEventSource } from '@microsoft/fetch-event-source'
import i18n from '../i18n'
import { useAuthStore } from '../stores/auth'
import { getNativeBridge } from '../native/bridge'
import { nativeRequestCancellation } from '../native/requestCancellation'
import type {
  AgentState,
  AppSettings,
  AppSettingsUpdate,
  ClaudeEnvState,
  CompletionEntry,
  ContextInfo,
  CreateRunResponse,
  Goal,
  MemoryListResponse,
  MemoryStatsResponse,
  PluginsResponse,
  QuotaResponse,
  RememberResponse,
  SessionSummary,
  StatsResponse,
  TranscriptMessage,
  WorkspaceFile,
} from '../types'

export interface StreamHandlers {
  onDelta: (text: string) => void
  /** Chain-of-thought fragment (extended thinking / reasoning_content). */
  onReasoning?: (text: string) => void
  /** Goal-loop narration (cycle banners, per-cycle chatter) — not an answer. */
  onGoalNote?: (text: string) => void
  onToolCall: (tc: { id: string; name: string; args: Record<string, unknown> }) => void
  onToolResult: (tr: { tool_call_id: string; content: string }) => void
  onInterrupt: (ev: { kind: string; payload: unknown }) => void
  /** An interrupt was answered (also replayed) — clear any pending card. */
  onInterruptResolved?: () => void
  onGoal?: (ev: { goal: Goal | null; running: boolean }) => void
  onRunQueued?: (position: number) => void
  onQueuePosition?: (position: number) => void
  onRunStarted?: () => void
  /** Replay gap (server retention window or client-detected seq jump). */
  onResync?: (reason: string) => void
  onDone: (cancelled: boolean) => void
  onError: (message: string) => void
}

// Shared skeleton for every non-SSE endpoint: on a non-2xx it prefers the
// JSON body's {error} detail (the Go server's writeError always sends one)
// over the bare status code, prefixed with a human-readable label so callers
// never surface a context-free number. Success bodies are always JSON; an
// empty body decodes to undefined for the void endpoints.
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

async function request<T>(url: string, init?: RequestInit, label?: string): Promise<T> {
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

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  return request<T>(
    url,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
    i18n.global.t('api.requestFailed'),
  )
}

export function createSession(workspace?: string): Promise<{
  session_id: string
  workspace?: string
  workspace_name?: string
}> {
  return postJSON('/api/sessions', { workspace: workspace ?? '' })
}

export function listSessions(): Promise<SessionSummary[]> {
  return request('/api/sessions', undefined, i18n.global.t('api.loadSessionsFailed'))
}

export function getSession(id: string): Promise<{ id: string; messages: TranscriptMessage[] }> {
  return request(`/api/sessions/${id}`, undefined, i18n.global.t('api.loadSessionsFailed'))
}

export function deleteSession(id: string): Promise<void> {
  return request(`/api/sessions/${id}`, { method: 'DELETE' }, i18n.global.t('api.deleteSessionFailed'))
}

export function getWorkspace(workspace?: string): Promise<WorkspaceFile | null> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/workspace${qs}`, undefined, i18n.global.t('api.loadWorkspaceFailed'))
}

/** Fetches one workspace file's content for the right-sidebar preview. */
export function getWorkspaceFile(path: string, workspace?: string): Promise<WorkspaceFile> {
  const ws = workspace ? `&workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/workspace/file?path=${encodeURIComponent(path)}${ws}`, undefined, i18n.global.t('api.readFileFailed'))
}

export function getSettings(): Promise<AppSettings> {
  return request('/api/settings', undefined, i18n.global.t('api.loadSettingsFailed'))
}

export function updateSettings(
  patch: AppSettingsUpdate,
): Promise<{ restart_required: boolean; apply_error?: string }> {
  return request('/api/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  }, i18n.global.t('api.saveSettingsFailed'))
}

/** Lists skills ("/" commands) and subagents ("@" mentions) for autocomplete. */
export function getCompletions(workspace?: string): Promise<{
  skills: CompletionEntry[]
  agents: CompletionEntry[]
}> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/completions${qs}`, undefined, i18n.global.t('api.loadCompletionsFailed'))
}

// --- runtime agent controls (permission mode / thinking / quota) ---

/** Live agent state for the chat-input toolbar. */
export function getAgent(): Promise<AgentState> {
  return request('/api/agent', undefined, i18n.global.t('api.loadAgentStateFailed'))
}

/** Switches the permission mode; takes effect for subsequent tool calls. */
export function setPermissionMode(mode: string): Promise<{ ok: boolean; mode: string }> {
  return postJSON('/api/agent/permissions', { mode })
}

/** Switches the thinking level (off | low | medium | high); applies next turn. */
export function setThinkingLevel(level: string): Promise<{
  ok: boolean
  level: string
  budget: number
}> {
  return postJSON('/api/agent/thinking', { level })
}

/** Provider account usage (plan quota / balance) + session usage fallback. */
export function getQuota(): Promise<QuotaResponse> {
  return request('/api/quota', undefined, i18n.global.t('api.loadQuotaFailed'))
}

/** GET /api/sessions/{id}/context: context-usage snapshot (hover-driven). */
export function getSessionContext(sessionId: string): Promise<ContextInfo> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/context`,
    undefined,
    i18n.global.t('api.loadContextUsageFailed'),
  )
}

// --- claude environment (Skill / MCP checkboxes) ---

/** Lists ~/.claude skills + global MCP servers and the current selection. */
export function getClaudeEnv(): Promise<ClaudeEnvState> {
  return request('/api/claudeenv', undefined, i18n.global.t('api.loadClaudeEnvFailed'))
}

/** Saves the checkbox selection; skills apply live, MCP at next agent start. */
export function putClaudeEnv(body: {
  skills: string[]
  mcp: string[]
}): Promise<{ ok: boolean; mcp_restart: boolean }> {
  return request('/api/claudeenv', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }, i18n.global.t('api.saveFailed'))
}

// --- plugins (settings panel section) ---

/** Lists built-in capability plugins with enabled + runtime install state. */
export function getPlugins(): Promise<PluginsResponse> {
  return request('/api/plugins', undefined, i18n.global.t('api.loadPluginsFailed'))
}

/** Persists the enabled plugin selection; the agent tool set rebinds live. */
export function putPlugins(enabled: string[]): Promise<{ ok: boolean }> {
  return request('/api/plugins', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  }, i18n.global.t('api.saveFailed'))
}

/** Starts an async runtime install; poll getPlugins() for progress. */
export function installPlugin(name: string, reinstall = false): Promise<{ ok: boolean }> {
  return request(
    `/api/plugins/${encodeURIComponent(name)}/${reinstall ? 'reinstall' : 'install'}`,
    { method: 'POST' },
    i18n.global.t('api.installStartFailed'),
  )
}

export function createRun(
  sessionId: string,
  prompt: string,
  workspace?: string,
  attachments?: Array<{ name: string; mime_type: string; data: string }>,
): Promise<CreateRunResponse> {
  return postJSON<CreateRunResponse>('/api/runs', {
    session_id: sessionId,
    prompt,
    workspace: workspace ?? '',
    attachments: attachments ?? [],
  })
}

export function answerRun(
  runId: string,
  body: { approve?: boolean; answers?: string[] },
): Promise<void> {
  return postJSON<void>(`/api/runs/${runId}/answer`, body)
}

export function cancelRun(runId: string): Promise<void> {
  return request(`/api/runs/${runId}/cancel`, { method: 'POST' }, i18n.global.t('api.cancelFailed'))
}

/** Buffers user input for a run in flight; attached after the current tool round. */
export function queueRunInput(runId: string, text: string): Promise<void> {
  return postJSON<void>(`/api/runs/${runId}/input`, { text })
}

/** Fetches a session's goal state (live, or the persisted sidecar after a restart). */
export function getGoal(sessionId: string): Promise<{ goal: Goal | null; running: boolean }> {
  return request(`/api/sessions/${sessionId}/goal`, undefined, i18n.global.t('api.loadGoalFailed'))
}

/**
 * Runs a /goal command server-side (no model turn). set/resume additionally
 * start the goal loop; a non-empty run_id means "subscribe to this run".
 */
export function sendGoalCommand(
  sessionId: string,
  command: string,
  workspace?: string,
): Promise<{ ok: boolean; output: string; goal: Goal | null; run_id: string }> {
  return postJSON(`/api/sessions/${sessionId}/goal`, { command, workspace: workspace ?? '' })
}

/** Lists (or searches with q) a workspace's memories. */
export function listMemory(opts: {
  workspace?: string
  q?: string
  layer?: string
  limit?: number
  offset?: number
}): Promise<MemoryListResponse> {
  const params = new URLSearchParams()
  if (opts.workspace) params.set('workspace', opts.workspace)
  if (opts.q) params.set('q', opts.q)
  if (opts.layer) params.set('layer', opts.layer)
  if (opts.limit) params.set('limit', String(opts.limit))
  if (opts.offset) params.set('offset', String(opts.offset))
  return request(`/api/memory?${params}`, undefined, i18n.global.t('api.loadMemoryFailed'))
}

export function memoryStats(workspace?: string): Promise<MemoryStatsResponse> {
  const params = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/memory/stats${params}`, undefined, i18n.global.t('api.loadMemoryStatsFailed'))
}

export type { StatsResponse } from '../types'

export function stats(): Promise<StatsResponse> {
  return request<StatsResponse>('/api/stats')
}

/** Deployment health; grafana_url appears only on server editions that
 *  set WOCA_GRAFANA_URL (design §12.3: the SPA embeds the IT pane link). */
export function health(): Promise<{ ok: boolean; starting?: boolean; grafana_url?: string }> {
  return request<{ ok: boolean; starting?: boolean; grafana_url?: string }>('/api/health')
}

/** Returns the new memory id, or '' when the attention gate dropped it. */
export function rememberMemory(body: {
  workspace?: string
  content: string
  tags?: string[]
}): Promise<RememberResponse> {
  return postJSON('/api/memory', body)
}

export function forgetMemory(id: string): Promise<void> {
  return request(`/api/memory/${encodeURIComponent(id)}`, { method: 'DELETE' }, i18n.global.t('api.deleteFailed'))
}

/*
 * run-events protocol (SSE): the server emits delta (answer text), reasoning
 * (thinking text), tool_call, tool_result, interrupt (permission/ask/plan
 * prompts), goal (goal-chip snapshot), run_queued / queue_position /
 * run_started (per-session admission gate), resync (the replay window can't
 * cover the gap — refetch a full snapshot) and the terminal done (carries
 * cancelled) / error (carries message) pair. Every event carries an id: line
 * with its per-run seq for Last-Event-ID resume.
 */
export function streamRun(
  runId: string,
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) return Promise.reject(streamAbortError())

  let lastSeq: number | null = null
  let attempts = 0
  const maxRetries = 5
  let terminal = false

  // Reconnect with Last-Event-ID: the server replays only the events we
  // missed (or sends `resync` when the gap exceeds its retention window),
  // so reconnecting a live run is now safe — the old "never reconnect"
  // assumption (see git history for the removed comment) no longer holds.
  const cancellation = nativeRequestCancellation(
    signal,
    lastSeq != null ? { 'Last-Event-ID': String(lastSeq) } : {},
  )
  const streamHeaders: Record<string, string> = { ...cancellation.headers }
  if (streamBridge) streamHeaders['X-Woca-Bridge'] = streamBridge
  if (streamAuth) streamHeaders.Authorization = `${streamAuth.scheme} ${streamAuth.token}`
  const connect = (): Promise<void> =>
    fetchEventSource(`${streamBase}/api/runs/${runId}/events`, {
      openWhenHidden: true,
      headers: streamHeaders,
      signal,
      async onopen(resp) {
        const ct = resp.headers.get('content-type') ?? ''
        if (!resp.ok || !ct.includes('text/event-stream')) {
          throw new Error(i18n.global.t('api.streamConnectFailed', { status: resp.status }))
        }
      },
      onmessage(ev) {
        attempts = 0 // reset the backoff counter on every received message
        if (ev.id) {
          const seq = Number(ev.id)
          if (lastSeq != null && seq > lastSeq + 1) {
            // seq jump: client-side fallback that triggers a replay when the server missed the gap
            handlers.onResync?.('seq_gap')
          }
          lastSeq = seq
        }
        if (!ev.data) return
        let data: unknown
        try {
          data = JSON.parse(ev.data)
        } catch {
          return
        }
        switch (ev.event) {
          case 'delta':
            handlers.onDelta((data as { text?: string }).text ?? '')
            break
          case 'reasoning':
            handlers.onReasoning?.((data as { text?: string }).text ?? '')
            break
          case 'goal_note':
            handlers.onGoalNote?.((data as { text?: string }).text ?? '')
            break
          case 'tool_call':
            handlers.onToolCall(data as { id: string; name: string; args: Record<string, unknown> })
            break
          case 'tool_result':
            handlers.onToolResult(data as { tool_call_id: string; content: string })
            break
          case 'interrupt':
            handlers.onInterrupt(data as { kind: string; payload: unknown })
            break
          case 'interrupt_resolved':
            handlers.onInterruptResolved?.()
            break
          case 'goal':
            handlers.onGoal?.(data as { goal: Goal | null; running: boolean })
            break
          case 'run_queued':
          case 'queue_position': {
            const pos = (data as { position?: number }).position ?? 0
            if (ev.event === 'run_queued') handlers.onRunQueued?.(pos)
            else handlers.onQueuePosition?.(pos)
            break
          }
          case 'run_started':
            handlers.onRunStarted?.()
            break
          case 'resync':
            handlers.onResync?.((data as { reason?: string }).reason ?? 'unknown')
            break
          case 'auth_revoked': {
            // Revocation closed the stream: surface the enumerated code
            // through the error channel; the next API probe raises the
            // login gate with the revoked message.
            terminal = true
            const code = (data as { code?: string }).code ?? 'unauthorized'
            handlers.onError(code === 'revoked' ? i18n.global.t('api.revoked') : i18n.global.t('api.streamConnectFailed', { status: 401 }))
            useAuthStore().raise()
            break
          }
          case 'done':
            terminal = true
            handlers.onDone((data as { cancelled?: boolean }).cancelled ?? false)
            break
          case 'error':
            terminal = true
            handlers.onError((data as { message?: string }).message ?? i18n.global.t('api.unknownError'))
            break
        }
      },
      onclose() {
        if (!terminal) throw new Error('stream closed before terminal event')
      },
      onerror(err) {
        if (terminal || ++attempts > maxRetries) throw err
        // Returning a number makes fetch-event-source retry after that many
        // milliseconds (reusing its built-in reconnection).
        return 1000
      },
    })

  let onAbort: (() => void) | undefined
  const streaming = new Promise<void>((resolve, reject) => {
    let settled = false
    onAbort = () => {
      if (settled) return
      settled = true
      reject(streamAbortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })

    let transport: Promise<void>
    try {
      transport = connect()
    } catch (error) {
      settled = true
      reject(error)
      return
    }
    void transport.then(
      () => {
        if (settled) return
        settled = true
        resolve()
      },
      (error) => {
        if (settled) return
        settled = true
        reject(error)
      },
    )
  })
  return streaming.finally(() => {
    if (onAbort) signal?.removeEventListener('abort', onAbort)
    cancellation.release()
  })
}

function streamAbortError(): DOMException {
  return new DOMException('The stream was aborted.', 'AbortError')
}

/*
 * Global event stream (GET /api/events, SSE): server-level session state
 * changes — {session_id, status: 'running' | 'awaiting' | '', run_id}. Every
 * connection is seeded with the current snapshot, so (re)connecting is itself
 * a resync; the browser EventSource reconnects automatically.
 */
export interface EventStream {
  close(): void
}

export function streamEvents(
  onSessionState: (ev: { session_id: string; status: string; run_id?: string }) => void,
): EventStream {
  const handleMessage = (data: string) => {
    try {
      onSessionState(JSON.parse(data))
    } catch {
      /* malformed frame — the next one re-syncs */
    }
  }

  if (!getNativeBridge()) {
    const es = new EventSource('/api/events')
    es.addEventListener('session_state', (ev) => handleMessage((ev as MessageEvent).data))
    return es
  }

  const controller = new AbortController()
  const cancellation = nativeRequestCancellation(controller.signal)
  const streaming = fetchEventSource('/api/events', {
    openWhenHidden: true,
    headers: cancellation.headers,
    signal: controller.signal,
    async onopen(response) {
      const contentType = response.headers.get('content-type') ?? ''
      if (!response.ok || !contentType.includes('text/event-stream')) {
        throw new Error(i18n.global.t('api.streamConnectFailed', { status: response.status }))
      }
    },
    onmessage(event) {
      if (event.event === 'session_state' && event.data) handleMessage(event.data)
    },
    onclose() {
      if (!controller.signal.aborted) throw new Error('global event stream closed')
    },
    onerror() {
      return 1000
    },
  })
  void streaming.catch(() => {}).finally(cancellation.release)
  return { close: () => controller.abort() }
}

// --- bridge product surface (login, folder browser, status) ---

export interface LoginResponse {
  token: string
  expires_at: number
  tenant_id: string
  user_id: string
  role: string
}

export function login(apiKey: string): Promise<LoginResponse> {
  return postJSON('/api/auth/login', { api_key: apiKey })
}

export function logout(): Promise<void> {
  return postJSON('/api/auth/logout', {})
}

export interface FsEntry {
  name: string
  path: string
  mtime: number
  writable: boolean
}

export interface FsListing {
  path: string
  parent: string
  dirs: FsEntry[]
  roots: string[]
}

export function fsList(path?: string): Promise<FsListing> {
  const q = path ? `?path=${encodeURIComponent(path)}` : ''
  return request(`/api/fs/list${q}`, undefined, i18n.global.t('api.fsListFailed'))
}

export function fsMkdir(parent: string, name: string): Promise<{ path: string }> {
  return postJSON('/api/fs/mkdir', { parent, name })
}

export interface FsValidate {
  exists: boolean
  isDir: boolean
  writable: boolean
}

export function fsValidate(path: string): Promise<FsValidate> {
  return request(`/api/fs/validate?path=${encodeURIComponent(path)}`, undefined, i18n.global.t('api.fsValidateFailed'))
}

export interface GatewayStatus {
  enrolled: boolean
  bridge_id?: string
  last_version?: string
  last_pull?: string
  stale: boolean
}

export interface TunnelStatus {
  serving: boolean
  served: number
}

export interface CloudAccessStatus {
  url: string
  url_source: string
  token_source: string
  token_set: boolean
  enrolled: boolean
  bridge_id?: string
  last_version?: string
  last_pull?: string
  stale: boolean
  spki_pin?: string
  spki_pinned: boolean
  tunnel: TunnelStatus
}

export interface BridgeStatus {
  edition: string
  /** Auth posture: "local" (loopback trust) or "apikey". */
  auth?: string
  /** Cloud-channel snapshot; absent on editions without the channel. */
  cloud?: CloudAccessStatus
  managed: boolean
}

export function getStatus(): Promise<BridgeStatus> {
  return request('/api/status', undefined, i18n.global.t('api.statusFailed'))
}

// --- bridge cloud access (design §6.5: the bridge-local config layer) ---

export function getCloudAccess(): Promise<CloudAccessStatus> {
  return request('/api/cloudaccess', undefined, i18n.global.t('api.requestFailed'))
}

export function updateCloudAccess(url: string, enrollmentToken: string): Promise<CloudAccessStatus> {
  return request(
    '/api/cloudaccess',
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, enrollment_token: enrollmentToken }),
    },
    i18n.global.t('api.requestFailed'),
  )
}

export function resetCloudAccess(): Promise<CloudAccessStatus> {
  return request(
    '/api/cloudaccess/reset',
    { method: 'POST' },
    i18n.global.t('api.requestFailed'),
  )
}

// --- edge access point (scenario ④: browser bridge switching) ---

export interface EdgeBridge {
  bridge_id: string
  created_at: number
  last_seen: number
  live: boolean
}

/**
 * Probes the same-origin edge inventory (GET /api/edge/bridges — the
 * edge's own pre-auth route, never parked into a tunnel). Returns null
 * unless the origin really is an edge access point: direct-bridge and
 * personal deployments answer an unknown /api path with the SPA shell
 * (200 HTML), which must not pass for an inventory.
 */
export async function getEdgeBridges(): Promise<EdgeBridge[] | null> {
  try {
    const resp = await fetch('/api/edge/bridges', {
      headers: { Accept: 'application/json' },
    })
    if (!resp.ok) return null
    const ct = resp.headers.get('Content-Type') ?? ''
    if (!ct.includes('application/json')) return null
    const data = (await resp.json()) as unknown
    if (!Array.isArray(data)) return null
    const out: EdgeBridge[] = []
    for (const item of data) {
      const b = item as Partial<EdgeBridge>
      if (typeof b.bridge_id !== 'string' || !b.bridge_id) continue
      out.push({
        bridge_id: b.bridge_id,
        created_at: typeof b.created_at === 'number' ? b.created_at : 0,
        last_seen: typeof b.last_seen === 'number' ? b.last_seen : 0,
        live: b.live === true,
      })
    }
    return out
  } catch {
    return null
  }
}

// --- managed delivery (远端配置: config-plane skills & MCP) ---

export interface ManagedSkill {
  name: string
  description?: string
}

export interface ManagedMCPServer {
  name: string
  type: string
  url?: string
  command?: string
}

export interface ManagedDelivery {
  skills: ManagedSkill[]
  mcp: ManagedMCPServer[]
}

/**
 * The operator-delivered skill/MCP set (read-only view of the managed
 * layer's materialized files; env values never cross the wire).
 */
export function getManagedDelivery(): Promise<ManagedDelivery> {
  return request('/api/managed', undefined, i18n.global.t('api.requestFailed'))
}

// --- model catalog (private-cloud form: pulled via the bridge) ---

export interface ModelPreset {
  id: string
  name: string
  provider: string
  model: string
  max_tokens?: number
  temperature?: number
  base_url?: string
}

export function listModels(): Promise<ModelPreset[]> {
  return request('/api/models', undefined, i18n.global.t('api.requestFailed'))
}

export function applyModel(id: string): Promise<{ ok: boolean; provider: string; model: string; apply_error?: string }> {
  return postJSON(`/api/models/${encodeURIComponent(id)}/apply`, {})
}

// --- bridge admin surface ---

export interface Me {
  user_id: string
  remark: string
  role: string
}

export function getMe(): Promise<Me> {
  return request('/api/auth/me', undefined, i18n.global.t('api.requestFailed'))
}

export interface BridgeSession {
  key_prefix: string
  remark: string
  role: string
  created_at: number
  last_seen: number
  expires_at: number
}

export function listBridgeSessions(): Promise<BridgeSession[]> {
  return request('/api/sessions/admin', undefined, i18n.global.t('api.requestFailed'))
}

export interface AuditRow {
  at: number
  actor: string
  action: string
  detail: string
}

export function listAudit(limit = 100): Promise<AuditRow[]> {
  return request(`/api/audit?limit=${limit}`, undefined, i18n.global.t('api.requestFailed'))
}
