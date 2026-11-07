// The activity panel's REST surface: URL/method/query construction and the
// typed error envelope. Branch decisions must use ApiError.code (never the
// HTTP status — ApiError carries none): S-B-0501 renders "unavailable in
// this deployment form", S-B-0004 is an unknown-or-foreign item.
import { afterEach, describe, expect, it, vi } from 'vitest'

import { deleteCron, deleteSession, getActivity, getShellOutput, getTaskOutput, killShell, stopTask } from './client'
import { ApiError } from './transport'

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

/** Stubs fetch with a 200 body; the mock keeps calls for URL assertions.
 * Each call gets a fresh Response — a body can only be consumed once. */
function okFetch(body: unknown = {}) {
  const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(jsonResponse(body)))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** Same, for an error envelope (fresh Response per call). */
function errorFetch(body: unknown, status: number) {
  const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(jsonResponse(body, status)))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('activity REST surface', () => {
  it('getActivity hits the snapshot route and resolves the typed body', async () => {
    const snapshot = {
      session_id: 's1',
      server_time: '2026-11-07T02:12:30Z',
      todos: { items: [], updated_at: null },
      shells: [],
      tasks: [],
      crons: [],
      wake: { enabled: true, capped: false, stalled: false, pending_notifications: 0 },
    }
    const fetchMock = okFetch(snapshot)
    await expect(getActivity('s 1')).resolves.toEqual(snapshot)
    expect(fetchMock).toHaveBeenCalledWith('/api/sessions/s%201/activity', undefined)
  })

  it('output tails encode ids, add tail_bytes only when asked, and share the shape', async () => {
    const fetchMock = okFetch({ id: '3', status: 'running', output: 'tail', truncated: true, total_bytes: 10 })
    await getShellOutput('s1', 'sh 2')
    expect(fetchMock.mock.calls[0][0]).toBe('/api/sessions/s1/background/shells/sh%202/output')
    await getShellOutput('s1', '3', 8192)
    expect(fetchMock.mock.calls[1][0]).toBe('/api/sessions/s1/background/shells/3/output?tail_bytes=8192')
    await getTaskOutput('s1', 'task-2', 4096)
    expect(fetchMock.mock.calls[2][0]).toBe('/api/sessions/s1/background/tasks/task-2/output?tail_bytes=4096')
    await expect(getTaskOutput('s1', 'task-2')).resolves.toMatchObject({ id: '3', truncated: true })
  })

  it('kill and stop are bare POSTs to their action routes', async () => {
    const fetchMock = okFetch({ shell: { id: '3' }, already_finished: false })
    await killShell('s1', '3')
    expect(fetchMock.mock.calls[0].slice(0, 2)).toEqual([
      '/api/sessions/s1/background/shells/3/kill',
      { method: 'POST' },
    ])
    await stopTask('s1', 'task-2')
    expect(fetchMock.mock.calls[1].slice(0, 2)).toEqual([
      '/api/sessions/s1/background/tasks/task-2/stop',
      { method: 'POST' },
    ])
  })

  it('cron delete is a DELETE and returns the deleted id', async () => {
    const fetchMock = okFetch({ deleted: true, id: 'cron-1' })
    await expect(deleteCron('s1', 'cron-1')).resolves.toEqual({ deleted: true, id: 'cron-1' })
    expect(fetchMock.mock.calls[0].slice(0, 2)).toEqual([
      '/api/sessions/s1/background/crons/cron-1',
      { method: 'DELETE' },
    ])
  })

  it('session delete resolves the teardown report', async () => {
    const fetchMock = okFetch({
      deleted: true,
      shells_killed: 2,
      tasks_stopped: 1,
      crons_deleted: 3,
      todos_dropped: true,
    })
    await expect(deleteSession('s1')).resolves.toEqual({
      deleted: true,
      shells_killed: 2,
      tasks_stopped: 1,
      crons_deleted: 3,
      todos_dropped: true,
    })
    expect(fetchMock.mock.calls[0].slice(0, 2)).toEqual(['/api/sessions/s1', { method: 'DELETE' }])
  })

  it('surfaces the standard error codes as typed ApiErrors', async () => {
    // Unknown or foreign item: 404 S-B-0004.
    errorFetch({ error: 'no such background item in this session', code: 'S-B-0004' }, 404)
    const missing = getActivity('s1')
    await expect(missing).rejects.toMatchObject({ name: 'ApiError', code: 'S-B-0004' })
    await expect(missing).rejects.toBeInstanceOf(ApiError)

    // Deployment form without the activity API: 501 S-B-0501.
    errorFetch({ error: 'session activity API is not available in this deployment form', code: 'S-B-0501' }, 501)
    await expect(killShell('s1', '3')).rejects.toMatchObject({ name: 'ApiError', code: 'S-B-0501' })
    await expect(stopTask('s1', 'task-2')).rejects.toMatchObject({ code: 'S-B-0501' })
    await expect(deleteCron('s1', 'cron-1')).rejects.toMatchObject({ code: 'S-B-0501' })
  })
})
