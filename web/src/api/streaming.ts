// The SSE engines: the per-run event stream (deltas, tool traffic,
// interrupts, admission-gate updates) and the global session-state stream.
import { fetchEventSource } from '@microsoft/fetch-event-source'
import { useAuthStore } from '../stores/auth'
import i18n from '../i18n'
import { getNativeBridge } from '../native/bridge'
import { nativeRequestCancellation } from '../native/requestCancellation'
import type {
  ActivityChangedPayload,
  AuthRevokedPayload,
  DonePayload,
  ErrorPayload,
  GoalPayload,
  InterruptPayload,
  QueuePositionPayload,
  ResyncPayload,
  RunStartedPayload,
  SSEEventType,
  SessionStatePayload,
  TextPayload,
  TimeoutPayload,
  ToolCallPayload,
  ToolResultPayload,
  WakeCappedPayload,
} from './events'
import { ACTIVITY_KINDS, type ActivityKind } from '../types'
import { streamTarget } from './transport'

export interface StreamHandlers {
  onDelta: (text: string) => void
  onReasoning?: (text: string) => void
  /** Goal-loop narration (cycle banners, per-cycle chatter) — not an answer. */
  onGoalNote?: (text: string) => void
  onToolCall: (tc: ToolCallPayload) => void
  onToolResult: (tr: ToolResultPayload) => void
  onInterrupt: (ev: InterruptPayload) => void
  /** An interrupt was answered (also replayed) — clear any pending card. */
  onInterruptResolved?: () => void
  /** The parked-interrupt TTL fired — cards flip to their timed-out state. */
  onTimeout?: (ev: TimeoutPayload) => void
  onGoal?: (ev: GoalPayload) => void
  onRunQueued?: (position: number) => void
  onQueuePosition?: (position: number) => void
  /** run_started — auto/trigger are present only on wake-driver runs. */
  onRunStarted?: (ev: RunStartedPayload) => void
  /** Replay gap (server retention window or client-detected seq jump). */
  onResync?: (reason: string) => void
  /** Terminal done — cancelled runs carry their typed stop cause alongside
   * the legacy flag (unknown/absent reason means the common user stop). */
  onDone: (cancelled: boolean, reason?: string) => void
  onError: (message: string) => void
}

/*
 * run-events protocol (SSE): the server emits delta (answer text), reasoning
 * (thinking text), tool_call, tool_result, interrupt (permission/ask/plan
 * prompts), timeout (parked-interrupt TTL — the terminal done follows), goal
 * (goal-chip snapshot), run_queued / queue_position / run_started
 * (per-session admission gate), resync (the replay window can't cover the
 * gap — refetch a full snapshot), auth_revoked (credential revoked
 * mid-stream) and the terminal done (carries cancelled plus the typed stop
 * cause reason) / error (carries message) pair. Event names and payload
 * shapes are the contract in
 * ./events.ts (mirrored from internal/server/ssevents.go; CI drift check).
 * Every event carries an id: line with its per-run seq for Last-Event-ID
 * resume.
 */
export function streamRun(
  runId: string,
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<void> {
  if (signal?.aborted) return Promise.reject(streamAbortError())

  const { base, bridge, auth } = streamTarget()
  let lastSeq: number | null = null
  let attempts = 0
  const maxRetries = 5
  let terminal = false

  // Reconnect with Last-Event-ID: the server replays only the events we
  // missed (or sends `resync` when the gap exceeds its retention window),
  // so reconnecting a live run is now safe — the old "never reconnect"
  // assumption (see git history for the removed comment) no longer holds.
  const cancellation = nativeRequestCancellation(
    signal,
    lastSeq != null ? { 'Last-Event-ID': String(lastSeq) } : {},
  )
  const streamHeaders: Record<string, string> = { ...cancellation.headers }
  if (bridge) streamHeaders['X-Woca-Bridge'] = bridge
  if (auth) streamHeaders.Authorization = `${auth.scheme} ${auth.token}`
  const connect = (): Promise<void> =>
    fetchEventSource(`${base}/api/runs/${runId}/events`, {
      openWhenHidden: true,
      headers: streamHeaders,
      signal,
      async onopen(resp) {
        const ct = resp.headers.get('content-type') ?? ''
        if (!resp.ok || !ct.includes('text/event-stream')) {
          throw new Error(i18n.global.t('api.streamConnectFailed', { status: resp.status }))
        }
      },
      onmessage(ev) {
        attempts = 0 // reset the backoff counter on every received message
        if (ev.id) {
          const seq = Number(ev.id)
          if (lastSeq != null && seq > lastSeq + 1) {
            // seq jump: client-side fallback that triggers a replay when the server missed the gap
            handlers.onResync?.('seq_gap')
          }
          lastSeq = seq
        }
        if (!ev.data) return
        let data: unknown
        try {
          data = JSON.parse(ev.data)
        } catch {
          return
        }
        switch (ev.event as SSEEventType) {
          case 'delta':
            handlers.onDelta((data as TextPayload).text ?? '')
            break
          case 'reasoning':
            handlers.onReasoning?.((data as TextPayload).text ?? '')
            break
          case 'goal_note':
            handlers.onGoalNote?.((data as TextPayload).text ?? '')
            break
          case 'tool_call':
            handlers.onToolCall(data as ToolCallPayload)
            break
          case 'tool_result':
            handlers.onToolResult(data as ToolResultPayload)
            break
          case 'interrupt':
            handlers.onInterrupt(data as InterruptPayload)
            break
          case 'interrupt_resolved':
            handlers.onInterruptResolved?.()
            break
          case 'timeout':
            // Parked-interrupt TTL fired; the server cancels the run and the
            // terminal done event closes the stream. Pending cards flip to
            // their persisted "timed out" state until then.
            handlers.onTimeout?.(data as TimeoutPayload)
            break
          case 'goal':
            handlers.onGoal?.(data as GoalPayload)
            break
          case 'run_queued':
          case 'queue_position': {
            const pos = (data as QueuePositionPayload).position ?? 0
            if (ev.event === 'run_queued') handlers.onRunQueued?.(pos)
            else handlers.onQueuePosition?.(pos)
            break
          }
          case 'run_started':
            handlers.onRunStarted?.(data as RunStartedPayload)
            break
          case 'resync':
            handlers.onResync?.((data as ResyncPayload).reason ?? 'unknown')
            break
          case 'auth_revoked': {
            // Revocation closed the stream: surface the enumerated code
            // through the error channel; the next API probe raises the
            // login gate with the revoked message.
            terminal = true
            const code = (data as AuthRevokedPayload).code ?? 'unauthorized'
            handlers.onError(code === 'revoked' ? i18n.global.t('api.revoked') : i18n.global.t('api.streamConnectFailed', { status: 401 }))
            useAuthStore().raise()
            break
          }
          case 'done': {
            terminal = true
            const done = data as DonePayload
            handlers.onDone(done.cancelled ?? false, done.reason)
            break
          }
          case 'error':
            terminal = true
            handlers.onError((data as ErrorPayload).message ?? i18n.global.t('api.unknownError'))
            break
        }
      },
      onclose() {
        if (!terminal) throw new Error('stream closed before terminal event')
      },
      onerror(err) {
        if (terminal || ++attempts > maxRetries) throw err
        // Returning a number makes fetch-event-source retry after that many
        // milliseconds (reusing its built-in reconnection).
        return 1000
      },
    })

  let onAbort: (() => void) | undefined
  const streaming = new Promise<void>((resolve, reject) => {
    let settled = false
    onAbort = () => {
      if (settled) return
      settled = true
      reject(streamAbortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })

    let transport: Promise<void>
    try {
      transport = connect()
    } catch (error) {
      settled = true
      reject(error)
      return
    }
    void transport.then(
      () => {
        if (settled) return
        settled = true
        resolve()
      },
      (error) => {
        if (settled) return
        settled = true
        reject(error)
      },
    )
  })
  return streaming.finally(() => {
    if (onAbort) signal?.removeEventListener('abort', onAbort)
    cancellation.release()
  })
}

function streamAbortError(): DOMException {
  return new DOMException('The stream was aborted.', 'AbortError')
}

/*
 * Global event stream (GET /api/events, SSE): server-level signals —
 * session_state ({session_id, status, run_id}), wake_capped (the session hit
 * its auto-wake chain budget) and activity_changed (signal-only: which
 * session's activity changed and which slice; clients refetch the snapshot
 * through the authorized REST route). Every connection is seeded with the
 * current session_state snapshot, so (re)connecting is itself a resync; the
 * browser EventSource reconnects automatically.
 */
export interface EventStream {
  close(): void
}

/** Consumer hook for the global stream; the activity store (F2) subscribes
 * to the activity/wake signals, onOpen re-pulls the current session. */
export interface GlobalEventHandlers {
  onSessionState: (ev: SessionStatePayload) => void
  /** Auto-wake chain budget hit — queued items wait for the next user run. */
  onWakeCapped?: (ev: WakeCappedPayload) => void
  /** A session's activity changed (todos | shells | tasks | crons | wake). */
  onActivityChanged?: (ev: ActivityChangedPayload) => void
  /** Fires on every (re)connection — the seeded snapshot is itself a resync,
   * so consumers refetch their current-session state (PM §4.4). */
  onOpen?: () => void
}

function isActivityKind(kind: unknown): kind is ActivityKind {
  return typeof kind === 'string' && (ACTIVITY_KINDS as readonly string[]).includes(kind)
}

/**
 * Dispatches one global-stream frame (event name + raw data). Malformed JSON
 * and payloads outside the contract are dropped — the next frame (or the
 * reconnect's seeded snapshot) re-syncs; consumer handler errors are not
 * caught here. Unknown activity_changed kinds are dropped too: the kind
 * union is closed at the type level, so a kind this client doesn't know
 * cannot be delivered as ActivityChangedPayload.
 */
function dispatchGlobalEvent(handlers: GlobalEventHandlers, name: string, data: string): void {
  let payload: unknown
  try {
    payload = JSON.parse(data)
  } catch {
    return // malformed frame — the next one re-syncs
  }
  if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) return
  switch (name) {
    case 'session_state':
      handlers.onSessionState(payload as SessionStatePayload)
      break
    case 'wake_capped':
      handlers.onWakeCapped?.(payload as WakeCappedPayload)
      break
    case 'activity_changed': {
      const ev = payload as ActivityChangedPayload
      if (!isActivityKind(ev.kind)) return
      handlers.onActivityChanged?.(ev)
      break
    }
  }
}

const GLOBAL_EVENT_NAMES = ['session_state', 'wake_capped', 'activity_changed'] as const

export function streamEvents(handlers: GlobalEventHandlers): EventStream {
  const onFrame = (name: string, data: string) => dispatchGlobalEvent(handlers, name, data)

  if (!getNativeBridge()) {
    const es = new EventSource('/api/events')
    for (const name of GLOBAL_EVENT_NAMES) {
      es.addEventListener(name, (ev) => onFrame(name, (ev as MessageEvent).data as string))
    }
    // EventSource fires 'open' on every (re)connection — the reconnect's
    // seeded snapshot is the resync trigger for consumers.
    es.addEventListener('open', () => handlers.onOpen?.())
    return es
  }

  const controller = new AbortController()
  const cancellation = nativeRequestCancellation(controller.signal)
  const streaming = fetchEventSource('/api/events', {
    openWhenHidden: true,
    headers: cancellation.headers,
    signal: controller.signal,
    async onopen(response) {
      const contentType = response.headers.get('content-type') ?? ''
      if (!response.ok || !contentType.includes('text/event-stream')) {
        throw new Error(i18n.global.t('api.streamConnectFailed', { status: response.status }))
      }
      handlers.onOpen?.()
    },
    onmessage(event) {
      if (event.data) onFrame(event.event, event.data)
    },
    onclose() {
      if (!controller.signal.aborted) throw new Error('global event stream closed')
    },
    onerror() {
      return 1000
    },
  })
  void streaming.catch(() => {}).finally(cancellation.release)
  return { close: () => controller.abort() }
}
