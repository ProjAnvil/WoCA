import { fetchEventSource } from '@microsoft/fetch-event-source'
import type {
  AgentState,
  AppSettings,
  AppSettingsUpdate,
  ClaudeEnvState,
  CompletionEntry,
  CreateRunResponse,
  Goal,
  MemoryListResponse,
  MemoryStatsResponse,
  PluginsResponse,
  QuotaResponse,
  RememberResponse,
  SessionSummary,
  TranscriptMessage,
  WorkspaceFile,
} from '../types'

export interface StreamHandlers {
  onDelta: (text: string) => void
  /** Chain-of-thought fragment (extended thinking / reasoning_content). */
  onReasoning?: (text: string) => void
  onToolCall: (tc: { id: string; name: string; args: Record<string, unknown> }) => void
  onToolResult: (tr: { tool_call_id: string; content: string }) => void
  onInterrupt: (ev: { kind: string; payload: unknown }) => void
  onGoal?: (ev: { goal: Goal | null; running: boolean }) => void
  onRunQueued?: (position: number) => void
  onQueuePosition?: (position: number) => void
  onRunStarted?: () => void
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
    '请求失败',
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
  return request('/api/sessions', undefined, '加载会话失败')
}

export function getSession(id: string): Promise<{ id: string; messages: TranscriptMessage[] }> {
  return request(`/api/sessions/${id}`, undefined, '加载会话失败')
}

export function deleteSession(id: string): Promise<void> {
  return request(`/api/sessions/${id}`, { method: 'DELETE' }, '删除会话失败')
}

export function getWorkspace(workspace?: string): Promise<WorkspaceFile | null> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/workspace${qs}`, undefined, '加载工作区失败')
}

/** Fetches one workspace file's content for the right-sidebar preview. */
export function getWorkspaceFile(path: string, workspace?: string): Promise<WorkspaceFile> {
  const ws = workspace ? `&workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/workspace/file?path=${encodeURIComponent(path)}${ws}`, undefined, '读取文件失败')
}

export function getSettings(): Promise<AppSettings> {
  return request('/api/settings', undefined, '加载设置失败')
}

export function updateSettings(
  patch: AppSettingsUpdate,
): Promise<{ restart_required: boolean; apply_error?: string }> {
  return request('/api/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  }, '保存设置失败')
}

/** Lists skills ("/" commands) and subagents ("@" mentions) for autocomplete. */
export function getCompletions(workspace?: string): Promise<{
  skills: CompletionEntry[]
  agents: CompletionEntry[]
}> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/completions${qs}`, undefined, '加载补全列表失败')
}

// --- runtime agent controls (permission mode / thinking / quota) ---

/** Live agent state for the chat-input toolbar. */
export function getAgent(): Promise<AgentState> {
  return request('/api/agent', undefined, '加载智能体状态失败')
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
  return request('/api/quota', undefined, '查询额度失败')
}

// --- claude environment (Skill / MCP checkboxes) ---

/** Lists ~/.claude skills + global MCP servers and the current selection. */
export function getClaudeEnv(): Promise<ClaudeEnvState> {
  return request('/api/claudeenv', undefined, '加载 Claude 环境失败')
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
  }, '保存失败')
}

// --- plugins (settings → 插件) ---

/** Lists built-in capability plugins with enabled + runtime install state. */
export function getPlugins(): Promise<PluginsResponse> {
  return request('/api/plugins', undefined, '加载插件列表失败')
}

/** Persists the enabled plugin selection; the agent tool set rebinds live. */
export function putPlugins(enabled: string[]): Promise<{ ok: boolean }> {
  return request('/api/plugins', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  }, '保存失败')
}

/** Starts an async runtime install; poll getPlugins() for progress. */
export function installPlugin(name: string, reinstall = false): Promise<{ ok: boolean }> {
  return request(
    `/api/plugins/${encodeURIComponent(name)}/${reinstall ? 'reinstall' : 'install'}`,
    { method: 'POST' },
    '安装启动失败',
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
  return request(`/api/runs/${runId}/cancel`, { method: 'POST' }, '取消失败')
}

/** Buffers user input for a run in flight; attached after the current tool round. */
export function queueRunInput(runId: string, text: string): Promise<void> {
  return postJSON<void>(`/api/runs/${runId}/input`, { text })
}

/** Fetches a session's goal state (live, or the persisted sidecar after a restart). */
export function getGoal(sessionId: string): Promise<{ goal: Goal | null; running: boolean }> {
  return request(`/api/sessions/${sessionId}/goal`, undefined, '加载目标失败')
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
  return request(`/api/memory?${params}`, undefined, '加载记忆失败')
}

export function memoryStats(workspace?: string): Promise<MemoryStatsResponse> {
  const params = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/memory/stats${params}`, undefined, '加载记忆统计失败')
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
  return request(`/api/memory/${encodeURIComponent(id)}`, { method: 'DELETE' }, '删除失败')
}

/*
 * run-events protocol (SSE): the server emits eleven event types —
 * delta (answer text), reasoning (thinking text), tool_call, tool_result,
 * interrupt (permission/ask/plan prompts), goal (goal-chip snapshot),
 * run_queued / queue_position / run_started (per-session admission gate:
 * a run queued behind another reports its 1-based position, then its
 * start once promoted), done (terminal; carries cancelled), error
 * (terminal, carries message). After done/error the server closes the
 * stream, so onclose needs no handling of its own. onerror rethrows
 * instead of letting fetch-event-source retry: a failed stream means the
 * run is gone or the terminal event was already consumed, and
 * reconnecting would just replay a dead run — the session store already
 * renders a terminal state from the thrown error.
 */
export function streamRun(runId: string, handlers: StreamHandlers): Promise<void> {
  return fetchEventSource(`/api/runs/${runId}/events`, {
    openWhenHidden: true,
    async onopen(resp) {
      const ct = resp.headers.get('content-type') ?? ''
      if (!resp.ok || !ct.includes('text/event-stream')) {
        throw new Error(`流连接失败：${resp.status}`)
      }
    },
    onmessage(ev) {
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
        case 'tool_call':
          handlers.onToolCall(data as { id: string; name: string; args: Record<string, unknown> })
          break
        case 'tool_result':
          handlers.onToolResult(data as { tool_call_id: string; content: string })
          break
        case 'interrupt':
          handlers.onInterrupt(data as { kind: string; payload: unknown })
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
        case 'done':
          handlers.onDone((data as { cancelled?: boolean }).cancelled ?? false)
          break
        case 'error':
          handlers.onError((data as { message?: string }).message ?? '未知错误')
          break
      }
    },
    onclose() {
      // The server closes the stream after a terminal `done`/`error` event.
    },
    onerror(err) {
      throw err
    },
  })
}
