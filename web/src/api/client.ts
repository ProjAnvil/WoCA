import { fetchEventSource } from '@microsoft/fetch-event-source'
import i18n from '../i18n'
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
async function request<T>(url: string, init?: RequestInit, label?: string): Promise<T> {
  const resp = await fetch(url, init)
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
  const connect = (): Promise<void> =>
    fetchEventSource(`/api/runs/${runId}/events`, {
      openWhenHidden: true,
      headers: cancellation.headers,
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
