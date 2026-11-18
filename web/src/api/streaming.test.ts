// The run-events dispatch switch: every SSE event name → handler routing,
// seq-gap detection, and the terminal/retry state machine.
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { fetchEventSource, getNativeBridge } = vi.hoisted(() => ({
  fetchEventSource: vi.fn(),
  getNativeBridge: vi.fn<() => unknown>(),
}))
vi.mock('@microsoft/fetch-event-source', () => ({ fetchEventSource }))
vi.mock('../native/bridge', () => ({ getNativeBridge }))

import { streamEvents, streamRun, type GlobalEventHandlers, type StreamHandlers } from './streaming'
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
    getNativeBridge.mockReturnValue(null)
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

  it('run_started carries the payload: auto/trigger on wake-driver runs', () => {
    const { h, options } = startRun()
    msg(options, 'run_started', { auto: true, trigger: 'notification' })
    msg(options, 'run_started', {})
    expect(h.onRunStarted).toHaveBeenNthCalledWith(1, { auto: true, trigger: 'notification' })
    expect(h.onRunStarted).toHaveBeenNthCalledWith(2, {})
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

  it('terminal events close the stream: done carries cancelled and reason, error the message', () => {
    const { h, options } = startRun()
    msg(options, 'error', { message: 'boom' })
    expect(h.onError).toHaveBeenCalledWith('boom')
    expect(() => options.onclose()).not.toThrow() // terminal: close is clean
    expect(() => options.onerror(new Error('late'))).toThrow('late') // no retry after terminal

    const second = startRun()
    msg(second.options, 'done', { cancelled: true })
    expect(second.h.onDone).toHaveBeenCalledWith(true, undefined)
    expect(() => second.options.onclose()).not.toThrow()

    // A stopped run's done carries its typed cause alongside the flag.
    const third = startRun()
    msg(third.options, 'done', { cancelled: true, reason: 'server_stop' })
    expect(third.h.onDone).toHaveBeenCalledWith(true, 'server_stop')

    const fourth = startRun()
    msg(fourth.options, 'done', { cancelled: false })
    expect(fourth.h.onDone).toHaveBeenCalledWith(false, undefined)
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

// --- global stream (GET /api/events): multi-event dispatch ---

/** Captures the listeners a browser EventSource registered. */
class FakeEventSource {
  static instances: FakeEventSource[] = []
  readonly url: string
  readonly listeners: Record<string, EventListener[]> = {}
  closed = false

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: EventListener) {
    ;(this.listeners[type] ??= []).push(listener)
  }

  close() {
    this.closed = true
  }
}

function emit(es: FakeEventSource, type: string, data: string) {
  for (const listener of es.listeners[type] ?? []) listener({ data } as MessageEvent)
}

describe('global event dispatch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    FakeEventSource.instances.length = 0
    fetchEventSource.mockReset().mockResolvedValue(undefined)
    getNativeBridge.mockReturnValue(null)
    setStreamBase('')
    setStreamBridge('')
    setStreamAuth('', '')
  })
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function globalHandlers() {
    return {
      onSessionState: vi.fn(),
      onWakeCapped: vi.fn(),
      onActivityChanged: vi.fn(),
      onOpen: vi.fn(),
    }
  }

  it('browser transport subscribes by name and routes each event to its handler', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const h = globalHandlers()
    streamEvents(h)
    const es = FakeEventSource.instances.at(-1)!
    expect(es.url).toBe('/api/events')
    expect(Object.keys(es.listeners).sort()).toEqual(['activity_changed', 'open', 'session_state', 'wake_capped'])

    emit(es, 'session_state', JSON.stringify({ session_id: 's1', status: 'running', run_id: 'r1' }))
    emit(es, 'wake_capped', JSON.stringify({ session: 's1', queued: 2 }))
    emit(es, 'activity_changed', JSON.stringify({ kind: 'shells', session_id: 's1' }))

    expect(h.onSessionState).toHaveBeenCalledWith({ session_id: 's1', status: 'running', run_id: 'r1' })
    expect(h.onWakeCapped).toHaveBeenCalledWith({ session: 's1', queued: 2 })
    expect(h.onActivityChanged).toHaveBeenCalledWith({ kind: 'shells', session_id: 's1' })
  })

  it('native transport dispatches the same events through fetchEventSource', () => {
    getNativeBridge.mockReturnValue({ id: 'native' })
    const h = globalHandlers()
    streamEvents(h)
    const options = fetchEventSource.mock.calls.at(-1)![1] as CapturedOptions

    options.onmessage({ event: 'session_state', data: JSON.stringify({ session_id: 's2', status: 'awaiting' }) })
    options.onmessage({ event: 'wake_capped', data: JSON.stringify({ session: 's2', queued: 5 }) })
    options.onmessage({ event: 'activity_changed', data: JSON.stringify({ kind: 'crons', session_id: 's2' }) })

    expect(h.onSessionState).toHaveBeenCalledWith({ session_id: 's2', status: 'awaiting' })
    expect(h.onWakeCapped).toHaveBeenCalledWith({ session: 's2', queued: 5 })
    expect(h.onActivityChanged).toHaveBeenCalledWith({ kind: 'crons', session_id: 's2' })
  })

  it('onOpen fires on every (re)open on both transports', async () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const browserHandlers = globalHandlers()
    streamEvents(browserHandlers)
    const es = FakeEventSource.instances.at(-1)!
    es.listeners.open[0](new Event('open'))
    es.listeners.open[0](new Event('open'))
    expect(browserHandlers.onOpen).toHaveBeenCalledTimes(2)

    getNativeBridge.mockReturnValue({ id: 'native' })
    const nativeHandlers = globalHandlers()
    streamEvents(nativeHandlers)
    const options = fetchEventSource.mock.calls.at(-1)![1] as CapturedOptions
    const opened = new Response('', { headers: { 'Content-Type': 'text/event-stream' } })
    await options.onopen(opened)
    await options.onopen(opened) // a reconnect is a fresh open
    expect(nativeHandlers.onOpen).toHaveBeenCalledTimes(2)
  })

  it('drops malformed frames and activity_changed kinds outside the typed union', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const h = globalHandlers()
    streamEvents(h)
    const es = FakeEventSource.instances.at(-1)!

    emit(es, 'session_state', '{not json')
    emit(es, 'session_state', 'null')
    emit(es, 'session_state', '5')
    emit(es, 'wake_capped', '[]')
    emit(es, 'activity_changed', JSON.stringify({ kind: 'snowflakes', session_id: 's1' })) // a server kind this client doesn't know
    emit(es, 'activity_changed', JSON.stringify({ session_id: 's1' })) // kind missing

    expect(h.onSessionState).not.toHaveBeenCalled()
    expect(h.onWakeCapped).not.toHaveBeenCalled()
    expect(h.onActivityChanged).not.toHaveBeenCalled()
  })

  it('delivers every documented activity_changed kind', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const h = globalHandlers()
    streamEvents(h)
    const es = FakeEventSource.instances.at(-1)!

    for (const kind of ['todos', 'shells', 'tasks', 'crons', 'wake']) {
      emit(es, 'activity_changed', JSON.stringify({ kind, session_id: 's1' }))
    }

    expect(h.onActivityChanged).toHaveBeenCalledTimes(5)
    expect(h.onActivityChanged).toHaveBeenLastCalledWith({ kind: 'wake', session_id: 's1' })
  })
})
