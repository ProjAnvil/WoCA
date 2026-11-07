// The session activity panel's REST surface (PM §4.3): one full-state
// snapshot, per-item output tails, and the user-initiated stops. The
// snapshot is the panel's single data shape — activity_changed signals
// only trigger a refetch, and branch-on-error must use ApiError.code (the
// error carries no HTTP status): S-B-0501 means "activity API not in this
// deployment form" (render unavailable, never an empty panel), S-B-0004
// unknown-or-foreign item.
import i18n from '../i18n'
import type { ActivitySnapshot, BackgroundShell, BackgroundTask, OutputTail } from '../types'
import { request } from './transport'

/** GET /api/sessions/{id}/activity — todos, shells, tasks, crons, wake. */
export function getActivity(sessionId: string): Promise<ActivitySnapshot> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/activity`,
    undefined,
    i18n.global.t('api.loadActivityFailed'),
  )
}

/** tail_bytes only rides along when the caller asks; absent = server default. */
const tailQuery = (tailBytes?: number) =>
  tailBytes != null ? `?tail_bytes=${tailBytes}` : ''

/** GET …/background/shells/{shell}/output — the shell's output tail. */
export function getShellOutput(sessionId: string, shellId: string, tailBytes?: number): Promise<OutputTail> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/background/shells/${encodeURIComponent(shellId)}/output${tailQuery(tailBytes)}`,
    undefined,
    i18n.global.t('api.loadOutputFailed'),
  )
}

/**
 * POST …/background/shells/{shell}/kill — the panel's user stop. The server
 * resolves the kill race: an already-finished shell answers 200 with
 * already_finished and its final state, never an error.
 */
export function killShell(
  sessionId: string,
  shellId: string,
): Promise<{ shell: BackgroundShell; already_finished: boolean }> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/background/shells/${encodeURIComponent(shellId)}/kill`,
    { method: 'POST' },
    i18n.global.t('api.killShellFailed'),
  )
}

/** GET …/background/tasks/{task}/output — streamed lines while running, the
 * final report after, "" when stopped. */
export function getTaskOutput(sessionId: string, taskId: string, tailBytes?: number): Promise<OutputTail> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/background/tasks/${encodeURIComponent(taskId)}/output${tailQuery(tailBytes)}`,
    undefined,
    i18n.global.t('api.loadOutputFailed'),
  )
}

/** POST …/background/tasks/{task}/stop — same race contract as killShell. */
export function stopTask(
  sessionId: string,
  taskId: string,
): Promise<{ task: BackgroundTask; already_finished: boolean }> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/background/tasks/${encodeURIComponent(taskId)}/stop`,
    { method: 'POST' },
    i18n.global.t('api.stopTaskFailed'),
  )
}

/** DELETE …/background/crons/{cron} — remove one scheduled prompt. */
export function deleteCron(
  sessionId: string,
  cronId: string,
): Promise<{ deleted: true; id: string }> {
  return request(
    `/api/sessions/${encodeURIComponent(sessionId)}/background/crons/${encodeURIComponent(cronId)}`,
    { method: 'DELETE' },
    i18n.global.t('api.deleteCronFailed'),
  )
}
