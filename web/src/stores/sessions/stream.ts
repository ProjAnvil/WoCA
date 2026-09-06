import { ref, type Ref } from 'vue'
import type { Goal, Interrupt, RunStatus, Session } from '../../types'
import { toErrorMessage } from '../../lib/errors'
import * as api from '../../api/client'
import type { ItemMutations } from './items'

function isAbortError(value: unknown): boolean {
  return typeof value === 'object' && value !== null && 'name' in value && value.name === 'AbortError'
}

export interface StreamDeps {
  demo: Ref<boolean>
  activeId: Ref<string>
  sessions: Ref<Session[]>
  status: Ref<Record<string, RunStatus>>
  interrupt: Ref<Record<string, Interrupt | null>>
  error: Ref<Record<string, string>>
  queuePosition: Ref<Record<string, number>>
  runId: Ref<Record<string, string>>
  // Text steered into the active run's live input (see sessions.ts steer());
  // cleared once the run reaches a terminal state.
  pendingSteers: Ref<Record<string, string[]>>
  streamingId: Ref<Record<string, string | null>>
  reasoningId: Ref<Record<string, string | null>>
  goalNoteId: Ref<Record<string, string | null>>
  goal: Ref<Record<string, Goal | null>>
  goalRunning: Ref<Record<string, boolean>>
  items: ItemMutations
  // Sends with a createRun POST in flight own the run they are about to
  // attach to — reattaching over them would reload the pre-run disk
  // snapshot and wipe the optimistic user bubble.
  sending: Set<string>
  loadSession: (id: string) => Promise<void>
  refreshContext: () => Promise<void>
}

/*
 * Run-stream plane: the server-truth run state map (sidebar badge +
 * refresh reattach), the global /api/events subscription, per-run SSE
 * attachment, and the replay-based reattach for runs this tab lost.
 */
export function createRunStreams(deps: StreamDeps) {
  const {
    demo,
    activeId,
    sessions,
    status,
    interrupt,
    error,
    queuePosition,
    runId,
    pendingSteers,
    streamingId,
    reasoningId,
    goalNoteId,
    goal,
    goalRunning,
    items,
    sending,
    loadSession,
    refreshContext,
  } = deps
  const { appendDelta, appendReasoning, appendGoalNote, addToolCall, addToolResult, closeStreaming, closeReasoning, closeGoalNote } = items

  // Server-truth run state per session (running / awaiting), seeded from the
  // session list and kept live by the global /api/events stream. This is the
  // authoritative signal for the sidebar badge — it survives page refreshes
  // and reflects runs other tabs started, unlike the local status map which
  // only covers streams this tab attached.
  const serverRunState = ref<Record<string, { status: 'running' | 'awaiting'; runId: string }>>({})
  let eventsSource: api.EventStream | null = null
  const runStreamControllers = new Map<string, AbortController>()
  const reattaching = new Set<string>()

  // runStateOf merges both signals: the local status map (instant, but only
  // for streams this tab attached) and the server state (authoritative,
  // survives refresh, covers other tabs). A dead local stream falls through
  // to the server value, so a run that outlived the stream still shows busy.
  function runStateOf(sid: string): 'running' | 'awaiting' | '' {
    const local = status.value[sid] ?? 'idle'
    if (local === 'awaiting') return 'awaiting'
    if (local === 'running' || local === 'queued') return 'running'
    return serverRunState.value[sid]?.status ?? ''
  }

  function seedRunState(list: { id: string; run_status?: string; run_id?: string }[]) {
    const next: Record<string, { status: 'running' | 'awaiting'; runId: string }> = {}
    for (const s of list) {
      if (s.run_status === 'running' || s.run_status === 'awaiting') {
        next[s.id] = { status: s.run_status, runId: s.run_id ?? '' }
      }
    }
    serverRunState.value = next
  }

  // connectGlobalEvents subscribes to /api/events. Native EventSource
  // reconnects on its own; every (re)connection is seeded with the current
  // snapshot server-side, which keeps the map fresh without polling.
  function connectGlobalEvents() {
    if (demo.value || eventsSource) return
    eventsSource = api.streamEvents((ev) => {
      if (ev.status === 'running' || ev.status === 'awaiting') {
        serverRunState.value = {
          ...serverRunState.value,
          [ev.session_id]: { status: ev.status, runId: ev.run_id ?? '' },
        }
        // A run started elsewhere (or before a refresh) in the visible
        // session: reattach its stream so the chat streams live again.
        if (ev.session_id === activeId.value) maybeReattach(ev.session_id)
      } else if (serverRunState.value[ev.session_id]) {
        const next = { ...serverRunState.value }
        delete next[ev.session_id]
        serverRunState.value = next
      }
    })
  }

  // maybeReattach resubscribes a session's live run after this tab lost its
  // stream (page refresh, or the run started in another tab). The replay
  // rebuilds the chat tail and restores a parked interrupt card.
  async function maybeReattach(sid: string) {
    const st = serverRunState.value[sid]
    if (!st || !st.runId || demo.value) return
    const local = status.value[sid] ?? 'idle'
    if (local === 'running' || local === 'queued' || local === 'awaiting') return
    // A send whose POST is still in flight owns the run it is about to
    // attach to — reattaching here would reload the pre-run snapshot over
    // its optimistic user bubble (see `sending` above).
    if (sending.has(sid)) return
    if (reattaching.has(st.runId)) return
    reattaching.add(st.runId)
    try {
      // Rebuild the item list from the on-disk transcript first. Runs
      // persist only at completion, so disk is always the pre-run snapshot —
      // replaying onto it appends exactly this run's events. This also
      // prevents duplicates when a broken earlier stream already applied
      // part of the run to the live items.
      const s = sessions.value.find((x) => x.id === sid)
      if (s) s.loaded = false
      interrupt.value[sid] = null
      await loadSession(sid)
      streamingId.value[sid] = null
      reasoningId.value[sid] = null
      goalNoteId.value[sid] = null
      await attachStream(sid, st.runId)
    } finally {
      reattaching.delete(st.runId)
    }
  }

  // attachStream subscribes to a run's SSE stream, updating this session's
  // chat items and goal chip. afterDone runs once the stream ends (used for
  // the goal loop's idle auto-continue after an interjected user run).
  //
  // claimRunId=false keeps runId[sid] on the run that currently owns the
  // session: a queued run must never steal the single answerInterrupt/cancel
  // slot (its interrupt-free lifetime makes the slot belong to the active
  // run by construction). The run_started handler claims the slot when a
  // queued run is promoted — at that point the previous run is guaranteed
  // terminal (the backend releases the gate only after persisting it).
  async function attachStream(
    sid: string,
    rid: string,
    afterDone?: () => void,
    claimRunId = true,
  ) {
    if (claimRunId) runId.value[sid] = rid
    runStreamControllers.get(rid)?.abort()
    const controller = new AbortController()
    runStreamControllers.set(rid, controller)
    try {
      await api.streamRun(rid, {
        onDelta: (t) => appendDelta(sid, t),
        onReasoning: (t) => appendReasoning(sid, t),
        onGoalNote: (t) => appendGoalNote(sid, t),
        onToolCall: (tc) => addToolCall(sid, tc),
        onToolResult: (tr) => addToolResult(sid, tr),
        onInterrupt: (ev) => {
          interrupt.value[sid] = { kind: ev.kind, payload: ev.payload } as unknown as Interrupt
          status.value[sid] = 'awaiting'
        },
        onInterruptResolved: () => {
          // Fired live on answer and on replay: a satisfied interrupt must
          // never linger as an unanswered card (the refresh-reattach path
          // replays answered interrupts too).
          interrupt.value[sid] = null
          if (status.value[sid] === 'awaiting') status.value[sid] = 'running'
        },
        onGoal: (ev) => {
          goal.value[sid] = ev.goal
          goalRunning.value[sid] = ev.running
        },
        onRunQueued(pos) {
          status.value[sid] = 'queued'
          queuePosition.value[sid] = pos
        },
        onQueuePosition(pos) {
          queuePosition.value[sid] = pos
        },
        onRunStarted() {
          status.value[sid] = 'running'
          delete queuePosition.value[sid]
          runId.value[sid] = rid
        },
        onResync() {
          // The gap exceeds the server's replay window (or a seq jump was
          // detected): fall back to refetching the full session messages.
          // loadSession skips already-loaded sessions, so reset loaded first
          // to force the refetch; after the rebuild the old streaming bubble
          // slots are stale — clear them so later deltas open fresh bubbles.
          const s = sessions.value.find((x) => x.id === sid)
          if (s) s.loaded = false
          void loadSession(sid).then(() => {
            streamingId.value[sid] = null
            reasoningId.value[sid] = null
          })
        },
        onDone: () => {
          closeStreaming(sid)
          closeReasoning(sid)
          closeGoalNote(sid)
          // A terminal event ends any parked interrupt too (TTL-cancelled
          // asks never get an interrupt_resolved of their own).
          interrupt.value[sid] = null
          status.value[sid] = 'done'
          delete queuePosition.value[sid]
          pendingSteers.value[sid] = []
          afterDone?.()
          // The terminal SSE event fires just before the server persists the
          // final transcript, so let the save land before re-reading it.
          if (sid === activeId.value) window.setTimeout(() => void refreshContext(), 500)
        },
        onError: (msg) => {
          closeStreaming(sid)
          closeReasoning(sid)
          closeGoalNote(sid)
          interrupt.value[sid] = null
          error.value[sid] = msg
          status.value[sid] = 'error'
          delete queuePosition.value[sid]
          pendingSteers.value[sid] = []
        },
      }, controller.signal)
    } catch (e) {
      if (controller.signal.aborted && isAbortError(e)) {
        if (runStreamControllers.get(rid) === controller) {
          closeStreaming(sid)
          closeReasoning(sid)
          closeGoalNote(sid)
        }
        return
      }
      closeStreaming(sid)
      closeReasoning(sid)
      closeGoalNote(sid)
      error.value[sid] = toErrorMessage(e)
      status.value[sid] = 'error'
      delete queuePosition.value[sid]
    } finally {
      if (runStreamControllers.get(rid) === controller) runStreamControllers.delete(rid)
    }
  }

  function abortRun(rid: string) {
    runStreamControllers.get(rid)?.abort()
  }

  return { serverRunState, runStateOf, seedRunState, connectGlobalEvents, maybeReattach, attachStream, abortRun }
}

export type RunStreams = ReturnType<typeof createRunStreams>
