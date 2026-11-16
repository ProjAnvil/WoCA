import { toErrorMessage } from '../lib/errors'
import { defineStore } from 'pinia'
import { ref } from 'vue'
import {
  deleteCron,
  getActivity,
  getShellOutput,
  getTaskOutput,
  killShell,
  stopTask,
} from '../api/client'
import { ApiError } from '../api/transport'
import type {
  ActivityChangedPayload,
  WakeCappedPayload,
} from '../api/events'
import type { ActivitySnapshot, OutputTail, TodoItem } from '../types'

// Trailing-debounce window for signal-driven snapshot refetches (PM §4.4:
// frames inside one window coalesce into a single GET).
const REFRESH_DEBOUNCE_MS = 200
// Max-wait cap for one pending debounce (F6): a signal cadence faster than
// the window (e.g. activity_changed every 150ms) would re-arm the trailing
// edge forever and postpone the fetch indefinitely; once a window has been
// pending this long, the next re-arm forces the fetch instead.
const REFRESH_MAX_WAIT_MS = 1000
// Output-tail polling cadence while an item runs and the page is visible.
const OUTPUT_POLL_MS = 2000

/** One session's cached activity snapshot plus its explicit UI states. */
export interface SessionActivityState {
  snapshot: ActivitySnapshot | null
  loading: boolean
  /** Last fetch/action failure, already localized by the API label. */
  error: string
  /** S-B-0501: the activity API is not served in this deployment form —
   * render "unavailable", never an empty list. */
  unavailable: boolean
  /** A signal for this session arrived while it was not active (or the
   * stream dropped): the snapshot may be outdated. */
  stale: boolean
}

/** Running counts the D2 delete-confirm lists before a destructive delete. */
export interface ActivityCounts {
  shells: number
  tasks: number
  crons: number
}

export type OutputKind = 'shell' | 'task'

const EMPTY_STATE: SessionActivityState = Object.freeze({
  snapshot: null,
  loading: false,
  error: '',
  unavailable: false,
  stale: false,
})

const outputKey = (kind: OutputKind, id: string) => `${kind}:${id}`

function emptySnapshot(sid: string): ActivitySnapshot {
  return {
    session_id: sid,
    server_time: new Date().toISOString(),
    todos: { items: [], updated_at: null },
    shells: [],
    tasks: [],
    crons: [],
    subagents: [],
    wake: { enabled: false, capped: false, stalled: false, pending_notifications: 0 },
  }
}

function failedIds(s: ActivitySnapshot): string[] {
  return [
    ...s.shells.filter((x) => x.status === 'failed').map((x) => x.id),
    ...s.tasks.filter((x) => x.status === 'failed').map((x) => x.id),
  ]
}

function countsOf(s: ActivitySnapshot): ActivityCounts {
  return {
    shells: s.shells.filter((x) => x.status === 'running').length,
    tasks: s.tasks.filter((x) => x.status === 'running').length,
    crons: s.crons.length,
  }
}

/*
 * Session activity panel store (PM §F2): per-session snapshot cache driven
 * by the global stream's signal-only events. Dependency direction is
 * deliberate — this store only touches the API and the sid it is handed;
 * stores/sessions/stream.ts subscribes it to /api/events so no cycle forms.
 * Event entry points (onActivityChanged / onWakeCapped / onStreamOpen) are
 * synchronous schedulers that never throw and never issue un-caught
 * requests: failures land in the session's explicit error state instead
 * (a throwing global-stream handler causes reconnect jitter on the native
 * transport, so the boundary catch lives here, where it is stored and
 * rendered — not swallowed).
 */
export const useActivityStore = defineStore('activity', () => {
  const bySession = ref<Record<string, SessionActivityState>>({})
  // Last output tail per item, written by watchOutput's poller (key
  // `${kind}:${id}`; ids are only unique per kind registry).
  const outputs = ref<Record<string, Record<string, OutputTail>>>({})
  const outputErrors = ref<Record<string, string>>({})
  // Failed items the user has not looked at since they failed (badge f).
  const unviewedFailures = ref<Record<string, number>>({})
  // Demo mode (no backend): serve empty snapshots, issue no requests.
  const demo = ref(false)

  // Inline per-item action errors (key `${sid}/${kind}:${id}`): rendered
  // next to the row that triggered them, so one failed stop/delete does not
  // paint the whole panel's error state (fetch errors stay there).
  const actionErrors = ref<Record<string, string>>({})
  // Sessions deleted server-side while a request was in flight: their
  // resolving responses must not resurrect a bySession entry (ghost state).
  const forgotten = new Set<string>()

  const debounceTimers = new Map<string, ReturnType<typeof setTimeout>>()
  // When the currently-pending debounce window OPENED (the first signal of
  // the burst) — the max-wait cap measures from here, not from each re-arm.
  const debounceStartedAt = new Map<string, number>()
  const watchers = new Map<string, Map<string, () => void>>()
  const viewedFailedIds = new Map<string, Set<string>>()

  function ensure(sid: string): SessionActivityState {
    let st = bySession.value[sid]
    if (!st) {
      st = { ...EMPTY_STATE }
      bySession.value[sid] = st
    }
    return st
  }

  function clearDebounce(sid: string) {
    const t = debounceTimers.get(sid)
    if (t) {
      clearTimeout(t)
      debounceTimers.delete(sid)
    }
    debounceStartedAt.delete(sid)
  }

  // applySnapshot installs a successful fetch: stale and error clear, the
  // unavailable flag resets (a forced refetch can land after a 501 flap),
  // and the unviewed-failure count catches items that failed while nobody
  // was watching. A fetch that started before forget(sid) resolves after it
  // must not resurrect the deleted session's entry.
  function applySnapshot(sid: string, snapshot: ActivitySnapshot) {
    if (forgotten.has(sid)) return
    const st = ensure(sid)
    st.snapshot = snapshot
    st.loading = false
    st.error = ''
    st.unavailable = false
    st.stale = false
    const failed = failedIds(snapshot)
    const viewed = viewedFailedIds.get(sid) ?? new Set<string>()
    unviewedFailures.value[sid] = failed.filter((id) => !viewed.has(id)).length
  }

  /** Immediate snapshot fetch (retry path / pre-action refresh). Catches
   * everything into the session's state — this is the store boundary. A
   * session forgotten while the GET was in flight stays forgotten. */
  async function fetchNow(sid: string): Promise<void> {
    if (forgotten.has(sid)) return
    if (demo.value) {
      applySnapshot(sid, emptySnapshot(sid))
      return
    }
    const st = ensure(sid)
    st.loading = true
    st.error = ''
    try {
      const snapshot = await getActivity(sid)
      if (forgotten.has(sid)) return
      applySnapshot(sid, snapshot)
    } catch (e) {
      if (forgotten.has(sid)) return
      st.loading = false
      if (e instanceof ApiError && e.code === 'S-B-0501') {
        // Not an error the user can retry away: this deployment form simply
        // does not serve the activity API. Snapshot stays null so the panel
        // renders "unavailable", never an empty list.
        st.unavailable = true
        st.snapshot = null
      } else {
        st.error = toErrorMessage(e)
      }
    }
  }

  /** Debounced (200ms trailing, capped at 1s max-wait) snapshot refetch for
   * one session — the activity_changed/wake_capped signal path. Each signal
   * re-arms the trailing edge, but never past REFRESH_MAX_WAIT_MS after the
   * window opened: a sustained sub-window signal cadence still lands a
   * fetch at the cap (the timer is clamped), and a signal arriving past the
   * cap forces the fetch immediately. Once a session is known
   * 501-unavailable, signals stop re-probing the absent endpoint. A
   * deliberate refresh re-interests the store in the sid (clears any
   * leftover forgotten marker — e.g. a recreated session id). */
  function refresh(sid: string): void {
    forgotten.delete(sid)
    if (demo.value) {
      applySnapshot(sid, emptySnapshot(sid))
      return
    }
    if (bySession.value[sid]?.unavailable) return
    const t = debounceTimers.get(sid)
    if (t) clearTimeout(t)
    // A pending window keeps its original open time (the cap is per burst,
    // not per signal); a first signal opens a fresh one.
    const startedAt = debounceStartedAt.get(sid) ?? Date.now()
    debounceStartedAt.set(sid, startedAt)
    const waited = Date.now() - startedAt
    const wait = Math.max(0, Math.min(REFRESH_DEBOUNCE_MS, REFRESH_MAX_WAIT_MS - waited))
    if (wait === 0) {
      // Cap reached: force the fetch now (leading edge after the cap).
      debounceTimers.delete(sid)
      debounceStartedAt.delete(sid)
      void fetchNow(sid)
      return
    }
    debounceTimers.set(
      sid,
      setTimeout(() => {
        debounceTimers.delete(sid)
        debounceStartedAt.delete(sid)
        void fetchNow(sid)
      }, wait),
    )
  }

  /**
   * activity_changed signal (D4): refetch the ACTIVE session only. A signal
   * for any other session marks its cache stale — the fetch happens when it
   * next becomes active (setActive refreshes unconditionally).
   */
  function onActivityChanged(ev: ActivityChangedPayload, activeId: string): void {
    if (ev.session_id === activeId) {
      refresh(ev.session_id)
      return
    }
    const st = bySession.value[ev.session_id]
    if (st) st.stale = true
  }

  /** wake_capped: the active session's wake block changed server-side
   * (capped just hit) — refetch so the snapshot reflects it (AC25). */
  function onWakeCapped(ev: WakeCappedPayload, activeId: string): void {
    if (ev.session === activeId) {
      refresh(ev.session)
      return
    }
    const st = bySession.value[ev.session]
    if (st) st.stale = true
  }

  /** Global stream (re)open: the seeded snapshot is itself a resync point —
   * refetch the active session immediately (not debounced) and drop any
   * signal still sitting in the debounce window (AC21). */
  function onStreamOpen(activeId: string): void {
    if (!activeId || demo.value) return
    if (bySession.value[activeId]?.unavailable) return
    clearDebounce(activeId)
    void fetchNow(activeId)
  }

  /** Boundary catch for the stream wiring: an unexpected synchronous throw
   * from an event handler is stored in the session's error channel instead
   * of escaping into the SSE transport (reconnect jitter). */
  function noteError(sid: string, e: unknown): void {
    ensure(sid).error = toErrorMessage(e)
  }

  // User actions: await the REST call, then re-read server truth — no
  // optimistic state (PM §F2). already_finished answers 200 with the final
  // state, so it flows through the same refetch; only real failures write
  // the item's inline action-error slot (and skip the refetch) — fetch
  // errors keep the panel-level session error.
  function clearActionError(sid: string, itemKey: string): void {
    const k = `${sid}/${itemKey}`
    if (actionErrors.value[k]) {
      const next = { ...actionErrors.value }
      delete next[k]
      actionErrors.value = next
    }
  }

  async function runAction(
    sid: string,
    itemKey: string,
    run: () => Promise<unknown>,
  ): Promise<void> {
    if (demo.value || forgotten.has(sid)) return
    clearActionError(sid, itemKey)
    try {
      await run()
    } catch (e) {
      actionErrors.value = { ...actionErrors.value, [`${sid}/${itemKey}`]: toErrorMessage(e) }
      return
    }
    await fetchNow(sid)
  }

  function killShellAction(sid: string, shellId: string): Promise<void> {
    return runAction(sid, `shell:${shellId}`, () => killShell(sid, shellId))
  }

  function stopTaskAction(sid: string, taskId: string): Promise<void> {
    return runAction(sid, `task:${taskId}`, () => stopTask(sid, taskId))
  }

  function deleteCronAction(sid: string, cronId: string): Promise<void> {
    return runAction(sid, `cron:${cronId}`, () => deleteCron(sid, cronId))
  }

  /**
   * Output-tail watcher (PM §F2): fetches the tail immediately, then every
   * 2s while the item runs and the page is visible. Stops when the caller
   * collapses the row (stop()), when the item reports a terminal status, or
   * when a read fails (no retry ladder — reopening restarts it). Hidden
   * pages skip ticks and resume once visible again. The last tail is always
   * readable through outputOf().
   */
  function watchOutput(sid: string, kind: OutputKind, id: string, tailBytes = 65536): { stop(): void } {
    const key = outputKey(kind, id)
    if (demo.value) return { stop: () => {} }
    // A watcher for this key may still be live (expand → collapse raced with
    // an in-flight poll, or a manual retry): stop it first or its interval
    // would keep firing with no handle left to stop it.
    watchers.get(sid)?.get(key)?.()
    const read = kind === 'shell' ? getShellOutput : getTaskOutput
    let timer: ReturnType<typeof setInterval> | null = null
    let stopped = false

    function stop() {
      if (stopped) return
      stopped = true
      if (timer) clearInterval(timer)
      // Only the registered handle may unregister itself — a superseded
      // handle (same key, older watcher) must not evict its replacement.
      if (watchers.get(sid)?.get(key) === stop) watchers.get(sid)!.delete(key)
    }

    async function poll(): Promise<void> {
      if (stopped || document.hidden) return
      let tail: OutputTail
      try {
        tail = await read(sid, id, tailBytes)
      } catch (e) {
        const errKey = `${sid}/${key}`
        outputErrors.value = { ...outputErrors.value, [errKey]: toErrorMessage(e) }
        stop()
        return
      }
      const errKey = `${sid}/${key}`
      if (outputErrors.value[errKey]) {
        const next = { ...outputErrors.value }
        delete next[errKey]
        outputErrors.value = next
      }
      const perSession = { ...(outputs.value[sid] ?? {}), [key]: tail }
      outputs.value = { ...outputs.value, [sid]: perSession }
      // A terminal status is the item's last word: one final tail, no more
      // ticks (AC23 — collapse / item end / hidden page all stop polling).
      if (tail.status !== 'running') stop()
    }

    let perSid = watchers.get(sid)
    if (!perSid) {
      perSid = new Map()
      watchers.set(sid, perSid)
    }
    perSid.set(key, stop)
    void poll()
    timer = setInterval(() => void poll(), OUTPUT_POLL_MS)
    return { stop }
  }

  /** Panel opened: the current failed set becomes the viewed baseline — the
   * danger badge resets; anything that fails afterwards counts anew. */
  function markViewed(sid: string): void {
    const ids = failedIds(bySession.value[sid]?.snapshot ?? emptySnapshot(sid))
    viewedFailedIds.set(sid, new Set(ids))
    unviewedFailures.value[sid] = 0
  }

  /**
   * D2 delete-confirm counts for the TARGET session (not just the active
   * one): served from a fresh cache when possible, fetched on demand
   * otherwise. Null = counts unknown (unavailable deployment or failed
   * read); the error itself is already stored in that session's state.
   */
  async function runningCountsFor(sid: string): Promise<ActivityCounts | null> {
    if (demo.value) return { shells: 0, tasks: 0, crons: 0 }
    const st = bySession.value[sid]
    if (st?.unavailable) return null
    if (st?.snapshot && !st.stale) return countsOf(st.snapshot)
    await fetchNow(sid)
    const after = bySession.value[sid]
    if (!after?.snapshot || after.unavailable || after.error) return null
    return countsOf(after.snapshot)
  }

  /** Drop every trace of a session (deleted server-side): cache, timers,
   * watchers and badge bookkeeping. In-flight requests for it resolve into
   * the forgotten set instead of resurrecting state. */
  function forget(sid: string): void {
    forgotten.add(sid)
    clearDebounce(sid)
    for (const stop of watchers.get(sid)?.values() ?? []) stop()
    watchers.delete(sid)
    viewedFailedIds.delete(sid)
    if (bySession.value[sid]) {
      const next = { ...bySession.value }
      delete next[sid]
      bySession.value = next
    }
    if (outputs.value[sid]) {
      const next = { ...outputs.value }
      delete next[sid]
      outputs.value = next
    }
    const errs = { ...outputErrors.value }
    let changed = false
    for (const k of Object.keys(errs)) {
      if (k.startsWith(`${sid}/`)) {
        delete errs[k]
        changed = true
      }
    }
    if (changed) outputErrors.value = errs
    const actionErrs = { ...actionErrors.value }
    let actionChanged = false
    for (const k of Object.keys(actionErrs)) {
      if (k.startsWith(`${sid}/`)) {
        delete actionErrs[k]
        actionChanged = true
      }
    }
    if (actionChanged) actionErrors.value = actionErrs
    if (unviewedFailures.value[sid] != null) {
      const next = { ...unviewedFailures.value }
      delete next[sid]
      unviewedFailures.value = next
    }
  }

  // --- read surface for the panel (functions over refs: reactive whenever
  // called from a computed/template, per-session so the store stays keyed) ---

  function stateOf(sid: string): SessionActivityState {
    return bySession.value[sid] ?? EMPTY_STATE
  }

  // The panel's data-state machine (UIUX §4.1): a session never seen is
  // LOADING, not empty — an empty list may only render from a snapshot.
  function statusOf(sid: string): 'loading' | 'ready' | 'unavailable' | 'error' {
    const st = bySession.value[sid]
    if (!st) return 'loading'
    if (st.unavailable) return 'unavailable'
    if (st.error) return 'error'
    if (st.loading || !st.snapshot) return 'loading'
    return 'ready'
  }

  function snapshotOf(sid: string): ActivitySnapshot | null {
    return bySession.value[sid]?.snapshot ?? null
  }

  /** Running background items (shells + tasks) — the badge count. */
  function runningCountOf(sid: string): number {
    const s = bySession.value[sid]?.snapshot
    if (!s) return 0
    return (
      s.shells.filter((x) => x.status === 'running').length +
      s.tasks.filter((x) => x.status === 'running').length
    )
  }

  function todoProgressOf(sid: string): { done: number; total: number; current: TodoItem | null } {
    const items = bySession.value[sid]?.snapshot?.todos.items ?? []
    return {
      done: items.filter((x) => x.status === 'completed').length,
      total: items.length,
      current: items.find((x) => x.status === 'in_progress') ?? null,
    }
  }

  function todoCountOf(sid: string): number {
    return bySession.value[sid]?.snapshot?.todos.items.length ?? 0
  }

  function backgroundCountOf(sid: string): number {
    const s = bySession.value[sid]?.snapshot
    return s ? s.shells.length + s.tasks.length : 0
  }

  function cronCountOf(sid: string): number {
    return bySession.value[sid]?.snapshot?.crons.length ?? 0
  }

  /** Still-active subagent children (running + waiting) — the Subagents
   * tab's badge count; finished rows stay visible without a count. */
  function subagentCountOf(sid: string): number {
    const s = bySession.value[sid]?.snapshot
    if (!s) return 0
    // Tolerate snapshots without the subagents slice (fixtures, and any
    // cached shape a mid-upgrade server might still hand out).
    return (s.subagents ?? []).filter((x) => x.state === 'running' || x.state === 'waiting').length
  }

  function unviewedFailureCountOf(sid: string): number {
    return unviewedFailures.value[sid] ?? 0
  }

  function outputOf(sid: string, kind: OutputKind, id: string): OutputTail | null {
    return outputs.value[sid]?.[outputKey(kind, id)] ?? null
  }

  function outputErrorOf(sid: string, kind: OutputKind, id: string): string {
    return outputErrors.value[`${sid}/${outputKey(kind, id)}`] ?? ''
  }

  /** The last failed action's message for one item ('' when none/unknown). */
  function actionErrorOf(sid: string, kind: OutputKind | 'cron', id: string): string {
    return actionErrors.value[`${sid}/${kind}:${id}`] ?? ''
  }

  function setDemo(v: boolean): void {
    demo.value = v
  }

  return {
    bySession,
    outputs,
    outputErrors,
    actionErrors,
    unviewedFailures,
    demo,
    refresh,
    fetchNow,
    onActivityChanged,
    onWakeCapped,
    onStreamOpen,
    noteError,
    killShell: killShellAction,
    stopTask: stopTaskAction,
    deleteCron: deleteCronAction,
    watchOutput,
    markViewed,
    runningCountsFor,
    forget,
    stateOf,
    statusOf,
    snapshotOf,
    runningCountOf,
    todoProgressOf,
    todoCountOf,
    backgroundCountOf,
    cronCountOf,
    subagentCountOf,
    unviewedFailureCountOf,
    outputOf,
    outputErrorOf,
    actionErrorOf,
    setDemo,
  }
})
