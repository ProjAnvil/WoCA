// The chat surface's REST calls: run lifecycle, runtime agent controls,
// memory, and the stats/health probes.
import i18n from '../i18n'
import type {
  AgentState,
  CreateRunResponse,
  MemoryListResponse,
  MemoryStatsResponse,
  QuotaResponse,
  RememberResponse,
  StatsResponse,
} from '../types'
import { postJSON, request } from './transport'

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
