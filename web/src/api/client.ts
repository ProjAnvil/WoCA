import { fetchEventSource } from '@microsoft/fetch-event-source'
import type {
  AppSettings,
  AppSettingsUpdate,
  CompletionEntry,
  Goal,
  MemoryListResponse,
  MemoryStatsResponse,
  RememberResponse,
  SessionSummary,
  TranscriptMessage,
  WorkspaceFile,
} from '../types'

export interface StreamHandlers {
  onDelta: (text: string) => void
  onToolCall: (tc: { id: string; name: string; args: Record<string, unknown> }) => void
  onToolResult: (tr: { tool_call_id: string; content: string }) => void
  onInterrupt: (ev: { kind: string; payload: unknown }) => void
  onGoal?: (ev: { goal: Goal | null; running: boolean }) => void
  onDone: (cancelled: boolean) => void
  onError: (message: string) => void
}

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  const resp = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!resp.ok) throw new Error(`请求失败：${resp.status}`)
  return (await resp.json()) as T
}

export function createSession(workspace?: string): Promise<{
  session_id: string
  workspace?: string
  workspace_name?: string
}> {
  return postJSON('/api/sessions', { workspace: workspace ?? '' })
}

export function listSessions(): Promise<SessionSummary[]> {
  return fetch('/api/sessions').then((r) => {
    if (!r.ok) throw new Error(`加载会话失败：${r.status}`)
    return r.json()
  })
}

export async function getSession(
  id: string,
): Promise<{ id: string; messages: TranscriptMessage[] }> {
  const resp = await fetch(`/api/sessions/${id}`)
  if (!resp.ok) throw new Error(`加载会话失败：${resp.status}`)
  return resp.json()
}

export async function getWorkspace(): Promise<WorkspaceFile | null> {
  const resp = await fetch('/api/workspace')
  if (!resp.ok) throw new Error(`加载工作区失败：${resp.status}`)
  return resp.json()
}

/** Fetches one workspace file's content for the right-sidebar preview. */
export async function getWorkspaceFile(path: string): Promise<WorkspaceFile> {
  const resp = await fetch(`/api/workspace/file?path=${encodeURIComponent(path)}`)
  if (!resp.ok) {
    const msg = await resp
      .json()
      .then((d) => (d as { error?: string }).error)
      .catch(() => '')
    throw new Error(msg || `读取文件失败：${resp.status}`)
  }
  return resp.json()
}

export async function getSettings(): Promise<AppSettings> {
  const resp = await fetch('/api/settings')
  if (!resp.ok) throw new Error(`加载设置失败：${resp.status}`)
  return resp.json()
}

export async function updateSettings(
  patch: AppSettingsUpdate,
): Promise<{ restart_required: boolean; apply_error?: string }> {
  const resp = await fetch('/api/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
  if (!resp.ok) {
    const msg = await resp
      .json()
      .then((d) => (d as { error?: string }).error)
      .catch(() => '')
    throw new Error(msg || `保存设置失败：${resp.status}`)
  }
  return resp.json()
}

/** Lists skills ("/" commands) and subagents ("@" mentions) for autocomplete. */
export async function getCompletions(): Promise<{
  skills: CompletionEntry[]
  agents: CompletionEntry[]
}> {
  const resp = await fetch('/api/completions')
  if (!resp.ok) throw new Error(`加载补全列表失败：${resp.status}`)
  return resp.json()
}

export function createRun(
  sessionId: string,
  prompt: string,
  workspace?: string,
  attachments?: Array<{ name: string; mime_type: string; data: string }>,
): Promise<{ run_id: string; session_id: string }> {
  return postJSON<{ run_id: string; session_id: string }>('/api/runs', {
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

export async function cancelRun(runId: string): Promise<void> {
  const resp = await fetch(`/api/runs/${runId}/cancel`, { method: 'POST' })
  if (!resp.ok) throw new Error(`取消失败：${resp.status}`)
}

/** Buffers user input for a run in flight; attached after the current tool round. */
export function queueRunInput(runId: string, text: string): Promise<void> {
  return postJSON<void>(`/api/runs/${runId}/input`, { text })
}

/** Fetches a session's goal state (live, or the persisted sidecar after a restart). */
export async function getGoal(
  sessionId: string,
): Promise<{ goal: Goal | null; running: boolean }> {
  const resp = await fetch(`/api/sessions/${sessionId}/goal`)
  if (!resp.ok) throw new Error(`加载目标失败：${resp.status}`)
  return resp.json()
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
export async function listMemory(opts: {
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
  const resp = await fetch(`/api/memory?${params}`)
  if (!resp.ok) throw new Error(`加载记忆失败：${resp.status}`)
  return resp.json()
}

export async function memoryStats(workspace?: string): Promise<MemoryStatsResponse> {
  const params = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  const resp = await fetch(`/api/memory/stats${params}`)
  if (!resp.ok) throw new Error(`加载记忆统计失败：${resp.status}`)
  return resp.json()
}

/** Returns the new memory id, or '' when the attention gate dropped it. */
export async function rememberMemory(body: {
  workspace?: string
  content: string
  tags?: string[]
}): Promise<RememberResponse> {
  return postJSON('/api/memory', body)
}

export async function forgetMemory(id: string): Promise<void> {
  const resp = await fetch(`/api/memory/${encodeURIComponent(id)}`, { method: 'DELETE' })
  if (!resp.ok) throw new Error(`删除失败：${resp.status}`)
}

/** Opens an SSE stream for a run and dispatches events to handlers. */
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
