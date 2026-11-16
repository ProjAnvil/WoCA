// The chat surface's REST calls: run lifecycle, runtime agent controls,
// memory, and the stats/health probes.
import i18n from '../i18n'
import type {
  AgentState,
  CardAnswerEntry,
  GrantInfo,
  PermissionMode,
  ThinkingLevel,
  CreateRunResponse,
  MemoryListResponse,
  MemoryStatsResponse,
  DictStatusResponse,
  QuotaResponse,
  RememberResponse,
  StatsResponse,
} from '../types'
import { postJSON, request } from './transport'

/** One parked inline review comment (rides the next run's prompt). */
export interface RunComment {
  file: string
  line: number
  text: string
}

export function createRun(
  sessionId: string,
  prompt: string,
  workspace?: string,
  attachments?: Array<{ name: string; mime_type: string; data: string }>,
  comments?: RunComment[],
): Promise<CreateRunResponse> {
  return postJSON<CreateRunResponse>('/api/runs', {
    session_id: sessionId,
    prompt,
    workspace: workspace ?? '',
    attachments: attachments ?? [],
    comments: comments ?? [],
  })
}

/** Starts a /btw side-question run: answered, never persisted to the transcript. */
export function startBtwRun(sessionId: string, prompt: string, workspace?: string): Promise<{ run_id: string }> {
  return postJSON<{ run_id: string }>(`/api/sessions/${encodeURIComponent(sessionId)}/side-runs`, {
    prompt,
    workspace: workspace ?? '',
  })
}

/** Rolls the conversation back to before the final user turn. */
export function dropLastTurn(sessionId: string): Promise<{ kept: number }> {
  return postJSON<{ kept: number }>(`/api/sessions/${encodeURIComponent(sessionId)}/truncate`, {
    drop_last_turn: true,
  })
}

/** Answers a parked interrupt. The body is either the list form
 * {answers: [{key, value}]} (any card count; entries may answer a subset —
 * unanswered cards re-park) or — for a single pending legacy card — the
 * scalar forms ({approve}, ask answers as a string list). */
export function answerRun(
  runId: string,
  body: { approve?: boolean; answers?: string[] | CardAnswerEntry[] },
): Promise<void> {
  return postJSON<void>(`/api/runs/${runId}/answer`, body)
}

export function cancelRun(runId: string): Promise<void> {
  return request(`/api/runs/${runId}/cancel`, { method: 'POST' }, i18n.global.t('api.cancelFailed'))
}

/** Steers an active run: appends text to the current turn's live input,
 * attached after its next tool round (Codex-style mid-turn steering). */
export function queueRunInput(runId: string, text: string): Promise<{ ok: boolean; queued: number }> {
  return postJSON(`/api/runs/${runId}/input`, { text })
}

// --- runtime agent controls (permission mode / thinking / quota) ---

/** Live agent state for the chat-input toolbar. */
export function getAgent(): Promise<AgentState> {
  return request('/api/agent', undefined, i18n.global.t('api.loadAgentStateFailed'))
}

/** Switches the permission mode; takes effect for subsequent tool calls.
 * The optional session scopes the switch to one session's policy (absent /
 * empty writes the base policy every session inherits — the pre-session
 * body shape, kept byte-identical for legacy callers). */
export function setPermissionMode(mode: PermissionMode, session?: string): Promise<{ ok: boolean; mode: string }> {
  return postJSON('/api/agent/permissions', session ? { mode, session } : { mode })
}

/** Switches the thinking level (off | low | medium | high); applies next turn. */
export function setThinkingLevel(level: ThinkingLevel): Promise<{
  ok: boolean
  level: string
  budget: number
}> {
  return postJSON('/api/agent/thinking', { level })
}

// --- session grants ("allow for session" rule keys, §3.3) ---

/** Lists the session's granted rule keys (the base's when session is ''). */
export function listGrants(session?: string): Promise<{ session: string; grants: GrantInfo[] }> {
  const params = session ? `?session=${encodeURIComponent(session)}` : ''
  return request(`/api/agent/grants${params}`, undefined, i18n.global.t('api.loadGrantsFailed'))
}

/** Revokes one session grant; revoking an absent key is a server-side no-op. */
export function revokeGrant(key: string, session?: string): Promise<void> {
  const params = new URLSearchParams({ key })
  if (session) params.set('session', session)
  return request(`/api/agent/grants?${params}`, { method: 'DELETE' }, i18n.global.t('api.revokeGrantFailed'))
}

/** Provider account usage (plan quota / balance) + session usage fallback. */
export function getQuota(): Promise<QuotaResponse> {
  return request('/api/quota', undefined, i18n.global.t('api.loadQuotaFailed'))
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

// --- memory (factual memory panel) ---

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

/** Reports the embedded engine's CJK tokenizer dictionary state. */
export function memoryDictStatus(): Promise<DictStatusResponse> {
  return request('/api/memory/dict', undefined, i18n.global.t('api.loadDictFailed'))
}

/** Downloads and installs the CJK dictionary (explicit user action; ~9MB). */
export function downloadMemoryDict(): Promise<DictStatusResponse> {
  return postJSON('/api/memory/dict', {})
}
