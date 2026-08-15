import { fetchEventSource } from '@microsoft/fetch-event-source'
import type { AppSettings, AppSettingsUpdate, SessionSummary, TranscriptMessage, WorkspaceFile } from '../types'

export interface StreamHandlers {
  onDelta: (text: string) => void
  onToolCall: (tc: { id: string; name: string; args: Record<string, unknown> }) => void
  onToolResult: (tr: { tool_call_id: string; content: string }) => void
  onInterrupt: (ev: { kind: string; payload: unknown }) => void
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

export function createSession(): Promise<string> {
  return postJSON<{ session_id: string }>('/api/sessions', {}).then((d) => d.session_id)
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

export function createRun(
  sessionId: string,
  prompt: string,
): Promise<{ run_id: string; session_id: string }> {
  return postJSON<{ run_id: string; session_id: string }>('/api/runs', {
    session_id: sessionId,
    prompt,
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
