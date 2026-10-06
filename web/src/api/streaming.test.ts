// The run-events dispatch switch: every SSE event name → handler routing,
// seq-gap detection, and the terminal/retry state machine.
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { fetchEventSource } = vi.hoisted(() => ({ fetchEventSource: vi.fn() }))
vi.mock('@microsoft/fetch-event-source', () => ({ fetchEventSource }))
vi.mock('../native/bridge', () => ({ getNativeBridge: () => null }))

import { streamRun, type StreamHandlers } from './streaming'
import { setStreamAuth, setStreamBase, setStreamBridge } from './transport'
import { useAuthStore } from '../stores/auth'
import i18n from '../i18n'

function handlers(): StreamHandlers {
  return {
    onDelta: vi.fn(),
    onReasoning: vi.fn(),
    onGoalNote: vi.fn(),
    onToolCall: vi.fn(),
    onToolResult: vi.fn(),
    onInterrupt: vi.fn(),
    onInterruptResolved: vi.fn(),
    onGoal: vi.fn(),
    onRunQueued: vi.fn(),
    onQueuePosition: vi.fn(),
    onRunStarted: vi.fn(),
    onResync: vi.fn(),
    onDone: vi.fn(),
    onError: vi.fn(),
  }
}

interface CapturedOptions {
  onopen: (resp: Response) => Promise<void>
  onmessage: (ev: { event?: string; data?: string; id?: string }) => void
  onclose: () => void
  onerror: (err: Error) => number | void
  headers: Record<string, string>
}

function startRun(h = handlers()): { h: any; options: CapturedOptions } {
  void streamRun('run-1', h).catch(() => {})
  const options = fetchEventSource.mock.calls.at(-1)![1] as CapturedOptions
  return { h, options }
}

const msg = (options: CapturedOptions, event: string, payload: unknown, id?: string) =>
  options.onmessage({ event, data: JSON.stringify(payload), id })

const sse = (body: string, init?: ResponseInit) =>
  new Response(body, { headers: { 'Content-Type': 'text/event-stream' }, ...init })

describe('run-events dispatch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    fetchEventSource.mockReset().mockResolvedValue(undefined)
    setStreamBase('')
    setStreamBridge('')
    setStreamAuth('', '')
  })

  it('routes every text-ish event to its handler with the payload text', () => {
    const { h, options } = startRun()
    msg(options, 'delta', { text: 'answer' })
    msg(options, 'reasoning', { text: 'thinking' })
    msg(options, 'goal_note', { text: 'cycle 1' })
    expect(h.onDelta).toHaveBeenCalledWith('answer')
    expect(h.onReasoning).toHaveBeenCalledWith('thinking')
    expect(h.onGoalNote).toHaveBeenCalledWith('cycle 1')
  })

  it('routes tool traffic, interrupts and lifecycle updates', () => {
    const { h, options } = startRun()
    msg(options, 'tool_call', { id: 'tc1', name: 'Bash', args: {} })
    msg(options, 'tool_result', { tool_call_id: 'tc1', content: 'ok' })
    msg(options, 'interrupt', { kind: 'permission', payload: {} })
    msg(options, 'interrupt_resolved', {})
    msg(options, 'goal', { goal: null, running: true })
    msg(options, 'run_queued', { position: 3 })
    msg(options, 'queue_position', { position: 1 })
    msg(options, 'run_started', {})
    msg(options, 'resync', { reason: 'retention' })
    expect(h.onToolCall).toHaveBeenCalledWith({ id: 'tc1', name: 'Bash', args: {} })
    expect(h.onToolResult).toHaveBeenCalledWith({ tool_call_id: 'tc1', content: 'ok' })
    expect(h.onInterrupt).toHaveBeenCalled()
    expect(h.onInterruptResolved).toHaveBeenCalled()
    expect(h.onGoal).toHaveBeenCalledWith({ goal: null, running: true })
    expect(h.onRunQueued).toHaveBeenCalledWith(3)
    expect(h.onQueuePosition).toHaveBeenCalledWith(1)
    expect(h.onRunStarted).toHaveBeenCalled()
    expect(h.onResync).toHaveBeenCalledWith('retention')
  })

  it('treats timeout as renderless and ignores empty or malformed frames', () => {
    const { h, options } = startRun()
    options.onmessage({ event: 'timeout', data: '{}' })
    options.onmessage({ event: 'delta', data: '' })
    options.onmessage({ event: 'delta', data: '{not json' })
    expect(h.onDelta).not.toHaveBeenCalled()
  })

  it('flags a seq jump as a client-side resync', () => {
    const { h, options } = startRun()
    msg(options, 'delta', { text: 'a' }, '1')
    msg(options, 'delta', { text: 'b' }, '5')
    expect(h.onResync).toHaveBeenCalledWith('seq_gap')
  })

  it('terminal events close the stream: done carries cancelled, error the message', () => {
    const { h, options } = startRun()
    msg(options, 'error', { message: 'boom' })
    expect(h.onError).toHaveBeenCalledWith('boom')
    expect(() => options.onclose()).not.toThrow() // terminal: close is clean
    expect(() => options.onerror(new Error('late'))).toThrow('late') // no retry after terminal

    const second = startRun()
    msg(second.options, 'done', { cancelled: true })
    expect(second.h.onDone).toHaveBeenCalledWith(true)
    expect(() => second.options.onclose()).not.toThrow()
  })

  it('auth_revoked surfaces the code and raises the login gate', () => {
    const auth = useAuthStore()
    const raised = vi.spyOn(auth, 'raise').mockImplementation(() => {})
    const { h, options } = startRun()
    msg(options, 'auth_revoked', { code: 'revoked' })
    expect(h.onError).toHaveBeenCalledWith(i18n.global.t('api.revoked'))
    expect(raised).toHaveBeenCalled()
    expect(() => options.onclose()).not.toThrow()
  })

  it('rejects non-SSE or failing opens', async () => {
    const { options } = startRun()
    await expect(
      options.onopen(new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })),
    ).rejects.toThrow()
    await expect(options.onopen(sse('', { status: 503 }))).rejects.toThrow()
  })

  it('a close before any terminal event is an error; retries back off to 1000ms', () => {
    const { options } = startRun()
    expect(() => options.onclose()).toThrow('stream closed before terminal event')
    expect(options.onerror(new Error('flake'))).toBe(1000)
    let last: unknown = 1000
    for (let i = 0; i < 4; i++) last = options.onerror(new Error('flake')) // 5 attempts total
    expect(last).toBe(1000)
    expect(() => options.onerror(new Error('given up'))).toThrow('given up') // 6th exceeds maxRetries
  })

  it('carries stream overrides: base without trailing slash, bridge header, auth', () => {
    setStreamBase('https://edge.example.com/')
    setStreamBridge('br-77')
    setStreamAuth('Bearer', 'tok-1')
    void streamRun('run-2', handlers()).catch(() => {})
    const [url, options] = fetchEventSource.mock.calls[0]
    expect(url).toBe('https://edge.example.com/api/runs/run-2/events')
    expect(options.headers['X-Woca-Bridge']).toBe('br-77')
    expect(options.headers.Authorization).toBe('Bearer tok-1')
  })
})
