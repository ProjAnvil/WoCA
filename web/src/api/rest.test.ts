// The thin REST wrappers: URL/method/body construction against a mocked
// transport core. One parametric suite covers chat, workspace and settings.
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { request, postJSON } = vi.hoisted(() => ({
  request: vi.fn().mockResolvedValue({}),
  postJSON: vi.fn().mockResolvedValue({}),
}))
vi.mock('./transport', () => ({ request, postJSON, UnauthorizedError: class {}, ApiError: class {} }))

import * as chat from './chat'
import * as workspace from './workspace'
import * as settings from './settings'
import './client' // executes the barrel re-exports (the stable import path)
import i18n from '../i18n'

const lastCall = (fn: typeof request) => fn.mock.calls.at(-1)!
const label = (key: string) => i18n.global.t(key)

beforeEach(() => {
  request.mockClear()
  postJSON.mockClear()
})

describe('chat REST surface', () => {
  it('createRun posts the envelope with defaulted empty collections', () => {
    void chat.createRun('s1', 'hi')
    expect(postJSON).toHaveBeenCalledWith('/api/runs', {
      session_id: 's1',
      prompt: 'hi',
      workspace: '',
      attachments: [],
      comments: [],
    })
  })

  it('createRun forwards workspace, attachments and comments verbatim', () => {
    void chat.createRun('s1', 'hi', '/ws', [{ name: 'a.png', mime_type: 'image/png', data: 'x' }], [
      { file: 'f.go', line: 3, text: 'here' },
    ])
    expect(postJSON.mock.calls[0][1]).toMatchObject({
      workspace: '/ws',
      attachments: [{ name: 'a.png' }],
      comments: [{ file: 'f.go', line: 3 }],
    })
  })

  it('side runs, truncation, answers, cancels and steering hit their routes', () => {
    void chat.startBtwRun('s 1', 'q', '/w')
    expect(postJSON).toHaveBeenCalledWith('/api/sessions/s%201/side-runs', {
      prompt: 'q',
      workspace: '/w',
    })
    void chat.dropLastTurn('s1')
    expect(postJSON).toHaveBeenCalledWith('/api/sessions/s1/truncate', { drop_last_turn: true })
    void chat.answerRun('r1', { approve: false, answers: ['x'] })
    expect(postJSON).toHaveBeenCalledWith('/api/runs/r1/answer', { approve: false, answers: ['x'] })
    void chat.cancelRun('r1')
    expect(request).toHaveBeenCalledWith('/api/runs/r1/cancel', { method: 'POST' }, label('api.cancelFailed'))
    void chat.queueRunInput('r1', 'steer')
    expect(postJSON).toHaveBeenCalledWith('/api/runs/r1/input', { text: 'steer' })
  })

  it('agent controls and probes', () => {
    void chat.getAgent()
    expect(request).toHaveBeenCalledWith('/api/agent', undefined, label('api.loadAgentStateFailed'))
    void chat.setPermissionMode('plan')
    expect(postJSON).toHaveBeenCalledWith('/api/agent/permissions', { mode: 'plan' })
    void chat.setThinkingLevel('high')
    expect(postJSON).toHaveBeenCalledWith('/api/agent/thinking', { level: 'high' })
    void chat.getQuota()
    expect(request).toHaveBeenCalledWith('/api/quota', undefined, label('api.loadQuotaFailed'))
    void chat.stats()
    expect(lastCall(request)[0]).toBe('/api/stats')
    void chat.health()
    expect(lastCall(request)[0]).toBe('/api/health')
  })

  it('memory listing builds the query string from present options only', () => {
    void chat.listMemory({ workspace: '/w', q: 'db', layer: 'facts', limit: 10, offset: 20 })
    expect(lastCall(request)[0]).toBe('/api/memory?workspace=%2Fw&q=db&layer=facts&limit=10&offset=20')
    void chat.listMemory({})
    expect(lastCall(request)[0]).toBe('/api/memory?')
    void chat.memoryStats('/w')
    expect(lastCall(request)[0]).toBe('/api/memory/stats?workspace=%2Fw')
    void chat.rememberMemory({ workspace: '/w', content: 'fact', tags: ['t'] })
    expect(postJSON).toHaveBeenCalledWith('/api/memory', { workspace: '/w', content: 'fact', tags: ['t'] })
    void chat.forgetMemory('mem/1')
    expect(request).toHaveBeenCalledWith('/api/memory/mem%2F1', { method: 'DELETE' }, label('api.deleteFailed'))
  })
})

describe('workspace REST surface', () => {
  it('session CRUD and transcripts', () => {
    void workspace.createSession('/w')
    expect(postJSON).toHaveBeenCalledWith('/api/sessions', { workspace: '/w' })
    void workspace.listSessions()
    expect(request).toHaveBeenCalledWith('/api/sessions', undefined, label('api.loadSessionsFailed'))
    void workspace.getSession('s1')
    expect(lastCall(request)[0]).toBe('/api/sessions/s1')
    void workspace.deleteSession('s1')
    expect(request).toHaveBeenCalledWith('/api/sessions/s1', { method: 'DELETE' }, label('api.deleteSessionFailed'))
  })

  it('workspace tree, file preview and changes carry the workspace parameter', () => {
    void workspace.getWorkspace('/w s')
    expect(lastCall(request)[0]).toBe('/api/workspace?workspace=%2Fw%20s')
    void workspace.getWorkspace()
    expect(lastCall(request)[0]).toBe('/api/workspace')
    void workspace.getWorkspaceFile('a b.go', '/w')
    expect(lastCall(request)[0]).toBe('/api/workspace/file?path=a%20b.go&workspace=%2Fw')
    void workspace.getWorkspaceChanges('/w')
    expect(lastCall(request)[0]).toBe('/api/workspace/changes?workspace=%2Fw')
    void workspace.undoWorkspace({ workspace: '/w', steps: 2 })
    expect(postJSON).toHaveBeenCalledWith('/api/workspace/undo', { workspace: '/w', steps: 2 })
  })

  it('completions, context, goals and the folder browser', () => {
    void workspace.getCompletions('/w')
    expect(lastCall(request)[0]).toBe('/api/completions?workspace=%2Fw')
    void workspace.getSessionContext('s 1')
    expect(lastCall(request)[0]).toBe('/api/sessions/s%201/context')
    void workspace.getGoal('s1')
    expect(lastCall(request)[0]).toBe('/api/sessions/s1/goal')
    void workspace.sendGoalCommand('s1', '/goal set x', '/w')
    expect(postJSON).toHaveBeenCalledWith('/api/sessions/s1/goal', {
      command: '/goal set x',
      workspace: '/w',
    })
    void workspace.fsList('/tmp')
    expect(lastCall(request)[0]).toBe('/api/filesystem/list?path=%2Ftmp')
    void workspace.fsMkdir('/tmp', 'sub')
    expect(postJSON).toHaveBeenCalledWith('/api/filesystem/mkdir', { parent: '/tmp', name: 'sub' })
  })
})

describe('settings REST surface', () => {
  it('settings reads and writes with the update envelope', async () => {
    void settings.getSettings()
    expect(request).toHaveBeenCalledWith('/api/settings', undefined, label('api.loadSettingsFailed'))
    void settings.updateSettings({ debug_log: true } as never)
    const [url, init] = lastCall(request)
    expect(url).toBe('/api/settings')
    expect(init.method).toBe('PUT')
    expect(JSON.parse(init.body)).toEqual({ debug_log: true })
  })

  it('claude environment selection round-trips', async () => {
    void settings.getClaudeEnv()
    expect(lastCall(request)[0]).toBe('/api/claude-env')
    void settings.putClaudeEnv({ skills: ['s'], mcp: ['m'] })
    const [, init] = lastCall(request)
    expect(init.method).toBe('PUT')
    expect(JSON.parse(init.body)).toEqual({ skills: ['s'], mcp: ['m'] })
  })
})
