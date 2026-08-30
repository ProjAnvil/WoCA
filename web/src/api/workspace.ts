// Session- and workspace-scoped REST: session CRUD, workspace file tree,
// completions, per-session goal state, and the folder browser.
import i18n from '../i18n'
import type {
  CompletionEntry,
  ContextInfo,
  Goal,
  SessionSummary,
  TranscriptMessage,
  WorkspaceFile,
} from '../types'
import { postJSON, request } from './transport'

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

/** One pending (undoable) workspace change with its unified diff. */
export interface WorkspaceChange {
  path: string
  added: number
  removed: number
  created: boolean
  deleted: boolean
  binary: boolean
  diff: string
}

export interface ChangesResponse {
  files: WorkspaceChange[]
  undo_steps: number
  redo_steps: number
}

/** The Changes review tab's payload: pending changes as diffs + step counts. */
export function getWorkspaceChanges(workspace?: string): Promise<ChangesResponse> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/workspace/changes${qs}`, undefined, i18n.global.t('api.loadChangesFailed'))
}

/** Checkpoint rollback: path-scoped (to before first touch) or N steps (0 = all). */
export function undoWorkspace(body: { workspace?: string; path?: string; steps?: number }): Promise<{
  applied: number
  summary: string
  undo_steps: number
  redo_steps: number
}> {
  return postJSON('/api/workspace/undo', body)
}

/** Lists skills ("/" commands) and subagents ("@" mentions) for autocomplete. */
export function getCompletions(workspace?: string): Promise<{
  skills: CompletionEntry[]
  agents: CompletionEntry[]
}> {
  const qs = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  return request(`/api/completions${qs}`, undefined, i18n.global.t('api.loadCompletionsFailed'))
}

/** GET /api/sessions/{id}/context: context-usage snapshot (hover-driven). */
export function getSessionContext(sessionId: string): Promise<ContextInfo> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/context`,
    undefined,
    i18n.global.t('api.loadContextUsageFailed'),
  )
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

// --- folder browser (bridge fsb surface) ---

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
  return request(`/api/filesystem/list${q}`, undefined, i18n.global.t('api.fsListFailed'))
}

export function fsMkdir(parent: string, name: string): Promise<{ path: string }> {
  return postJSON('/api/filesystem/mkdir', { parent, name })
}
