import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type {
  Attachment,
  ChatItem,
  Goal,
  Interrupt,
  RunStatus,
  Session,
  WorkspaceGroup,
} from '../types'
import * as api from '../api/client'
import i18n from '../i18n'
import { truncate, baseName } from '../lib/format'
import { messagesToItems, nid } from '../lib/transcript'
import { useAgentStore, isPermissionMode, isThinkingLevel } from './agent'
import { toErrorMessage } from '../lib/errors'
import { createItemMutations } from './sessions/items'
import { createContextTracker } from './sessions/context'
import { createRunStreams } from './sessions/stream'

// demoReply is a canned assistant answer used when the store runs in demo mode
// (no backend), so the standalone demo remains interactive.
function demoReply(prompt: string): string {
  return [
    i18n.global.t('sessions.demo.reply', { prompt: truncate(prompt, 24) }),
    '',
    '```',
    i18n.global.t('sessions.demo.codeBody'),
    '```',
    '',
    i18n.global.t('sessions.demo.realMode'),
    i18n.global.t('sessions.demo.toolCards'),
  ].join('\n')
}

// Orchestrating session store: list/selection/workspace grouping plus the
// send/goal/interrupt lifecycle. The three planes live in ./sessions/*:
// items.ts (live-block mutations), context.ts (usage snapshot), stream.ts
// (server run state + SSE attach/reattach).
export const useSessionStore = defineStore('sessions', () => {
  const sessions = ref<Session[]>([])
  const activeId = ref('')

  const items = ref<Record<string, ChatItem[]>>({})
  // True while a session's transcript is being fetched (message skeletons).
  const historyLoading = ref(false)
  const status = ref<Record<string, RunStatus>>({})
  // 1-based queue position while a run waits on the session gate (backend
  // admission; absent = not queued).
  const queuePosition = ref<Record<string, number>>({})
  const error = ref<Record<string, string>>({})
  const runId = ref<Record<string, string>>({})
  const interrupt = ref<Record<string, Interrupt | null>>({})
  // Text submitted via send() while a run is active: queued into the live
  // run's input immediately (Codex-style steering) rather than waiting for
  // it to finish. Cleared on completion; resubmitted as a fresh turn if the
  // run is canceled before it lands (see cancel()).
  const pendingSteers = ref<Record<string, string[]>>({})
  const streamingId = ref<Record<string, string | null>>({})
  // The reasoning (thinking) item currently streaming for a session.
  const reasoningId = ref<Record<string, string | null>>({})
  // The goal-loop narration block currently streaming for a session (cycle
  // banners + per-cycle chatter; never an answer bubble).
  const goalNoteId = ref<Record<string, string | null>>({})
  // Goal mode: per-session goal snapshot + whether the goal loop is running.
  const goal = ref<Record<string, Goal | null>>({})
  const goalRunning = ref<Record<string, boolean>>({})
  const goalPanelOpen = ref(false)

  // --- demo mode (the standalone demo app seeds this instead of the backend) ---
  const demo = ref(false)

  const mutations = createItemMutations({ items, streamingId, reasoningId, goalNoteId, runId })
  const { list, push, closeStreaming, closeReasoning, closeGoalNote } = mutations

  const { contextInfo, contextLoading, contextError, refreshContext } = createContextTracker({
    activeId,
    items,
    demo,
  })

  // Sessions with a createRun POST in flight. The server's global `running`
  // event races the POST response; without this marker maybeReattach would
  // treat our own just-started run as a foreign one (local status is still
  // the previous turn's) and rebuild items from the pre-run disk snapshot,
  // wiping the optimistic user bubble send() just pushed.
  const sending = new Set<string>()

  const streams = createRunStreams({
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
    items: mutations,
    sending,
    loadSession: (id) => loadSession(id),
    refreshContext: () => refreshContext(),
  })
  const { serverRunState, runStateOf, seedRunState, connectGlobalEvents, maybeReattach, attachStream, abortRun } = streams

  const activeSession = computed(() => sessions.value.find((s) => s.id === activeId.value))
  const activeItems = computed(() => items.value[activeId.value] ?? [])
  const activeStatus = computed(() => status.value[activeId.value] ?? 'idle')
  const activeError = computed(() => error.value[activeId.value] ?? '')
  const activeInterrupt = computed(() => interrupt.value[activeId.value] ?? null)
  const activeGoal = computed(() => goal.value[activeId.value] ?? null)
  const activePendingSteers = computed(() => pendingSteers.value[activeId.value] ?? [])

  // Sessions grouped by workspace (list is newest-first, so groups are too).
  // The key is the workspace path when known, falling back to its name.
  const workspaceGroups = computed<WorkspaceGroup[]>(() => {
    const groups: WorkspaceGroup[] = []
    const byKey = new Map<string, WorkspaceGroup>()
    for (const s of sessions.value) {
      const path = s.workspace ?? ''
      const name = s.workspaceName || (path ? baseName(path) : '') || i18n.global.t('sessions.defaultWorkspace')
      const key = path || `name:${name}`
      let g = byKey.get(key)
      if (!g) {
        g = { key, path, name, sessions: [] }
        byKey.set(key, g)
        groups.push(g)
      }
      g.sessions.push(s)
    }
    return groups
  })

  const activeWorkspace = computed(() => {
    const s = activeSession.value
    if (s) {
      return { path: s.workspace ?? '', name: s.workspaceName || (s.workspace ? baseName(s.workspace) : '') || i18n.global.t('sessions.defaultWorkspace') }
    }
    const g = workspaceGroups.value[0]
    return g ? { path: g.path, name: g.name } : { path: '', name: i18n.global.t('sessions.defaultWorkspace') }
  })

  const activeWorkspaceKey = computed(() => {
    const ws = activeWorkspace.value
    return ws.path || `name:${ws.name}`
  })

  function ensureMaps(id: string) {
    if (!items.value[id]) items.value[id] = []
    if (!status.value[id]) status.value[id] = 'idle'
    if (!interrupt.value[id]) interrupt.value[id] = null
    if (!streamingId.value[id]) streamingId.value[id] = null
    if (!(id in goal.value)) goal.value[id] = null
    if (!(id in goalRunning.value)) goalRunning.value[id] = false
    if (!(id in goalNoteId.value)) goalNoteId.value[id] = null
  }

  function setDemo(v: boolean) {
    demo.value = v
  }
  function seed(seedSessions: Session[], itemsMap: Record<string, ChatItem[]>) {
    sessions.value = seedSessions.map((s) => ({
      ...s,
      workspace: s.workspace ?? '',
      workspaceName: s.workspaceName ?? '',
    }))
    items.value = { ...itemsMap }
    if (seedSessions.length) {
      activeId.value = seedSessions[0].id
      for (const s of seedSessions) ensureMaps(s.id)
    }
  }
  function triggerInterrupt(intr: Interrupt) {
    const sid = activeId.value
    if (!sid) return
    interrupt.value[sid] = intr
    status.value[sid] = 'awaiting'
  }

  async function init() {
    try {
      const list = await api.listSessions()
      if (list.length === 0) {
        await createSession()
        return
      }
      sessions.value = list.map((s) => ({
        id: s.id,
        title: s.title,
        updatedAt: s.updated_at,
        loaded: false,
        workspace: s.workspace ?? '',
        workspaceName: s.workspace_name ?? (s.workspace ? baseName(s.workspace) : i18n.global.t('sessions.defaultWorkspace')),
      }))
      activeId.value = sessions.value[0].id
      seedRunState(list)
      connectGlobalEvents()
      await loadSession(sessions.value[0].id)
      // A run still in flight from before the refresh: stream it again.
      maybeReattach(activeId.value)
    } catch {
      await createSession()
    }
  }

  async function createSession(workspace?: { path?: string; name?: string }) {
    const wsPath = workspace?.path ?? activeWorkspace.value.path
    const wsName = workspace?.name ?? activeWorkspace.value.name
    let id = ''
    let path = wsPath
    let name = wsName
    try {
      const resp = await api.createSession(wsPath)
      id = resp.session_id
      // Prefer the server's resolved values (it expands ~ to an absolute path
      // and derives the display name) so later runs reuse the same workspace.
      path = resp.workspace ?? wsPath
      name = resp.workspace_name ?? wsName
    } catch {
      id = `tmp-${nid()}`
    }
    sessions.value = [
      { id, title: i18n.global.t('sessions.newSession'), updatedAt: '', loaded: true, workspace: path, workspaceName: name },
      ...sessions.value,
    ]
    ensureMaps(id)
    activeId.value = id
    // A fresh session starts empty: clear the previous session's snapshot so
    // the context chip shows its disabled state, not stale numbers.
    contextInfo.value = null
    contextError.value = ''
  }

  async function loadSession(id: string) {
    const s = sessions.value.find((x) => x.id === id)
    if (!s) return
    ensureMaps(id)
    if (!s.loaded) {
      historyLoading.value = true
      try {
        const { messages } = await api.getSession(id)
        items.value[id] = messagesToItems(messages)
        s.loaded = true
      } catch {
        items.value[id] = []
      } finally {
        historyLoading.value = false
      }
    }
    // Refresh the goal chip (sidecar-backed after a server restart).
    api.getGoal(id).then((g) => {
      goal.value[id] = g.goal
      goalRunning.value[id] = g.running
    }).catch(() => {})
    // Context snapshot once this session's items are loaded (the empty-check
    // inside needs the final transcript, not the pre-load state).
    if (id === activeId.value) void refreshContext()
  }

  async function setActive(id: string) {
    activeId.value = id
    await loadSession(id)
    // Switching onto a session whose run lives on the server (started in
    // another tab / before a refresh): pick its stream back up.
    maybeReattach(id)
  }

  // removeSession deletes a session server-side and drops all of its local
  // state. Deleting the active session switches to the newest remaining
  // session of the same workspace (else the newest overall, else a fresh one).
  // A session with a live run refuses — stop it first.
  async function removeSession(id: string) {
    const st = status.value[id] ?? 'idle'
    if (st === 'running' || st === 'awaiting' || st === 'queued') {
      throw new Error(i18n.global.t('sessions.sessionBusyDeleteFirst'))
    }
    if (!demo.value) await api.deleteSession(id)
    const target = sessions.value.find((s) => s.id === id)
    const wsPath = target?.workspace ?? ''
    sessions.value = sessions.value.filter((s) => s.id !== id)
    for (const m of [items, status, queuePosition, error, runId, interrupt, streamingId, reasoningId, goalNoteId, goal, goalRunning]) {
      delete m.value[id]
    }
    if (serverRunState.value[id]) {
      const next = { ...serverRunState.value }
      delete next[id]
      serverRunState.value = next
    }
    if (activeId.value === id) {
      const next =
        sessions.value.find((s) => s.workspace === wsPath) ?? sessions.value[0]
      if (next) await setActive(next.id)
      else await createSession(wsPath ? { path: wsPath } : undefined)
    }
  }

  /*
   * Idle continuation: while a goal is active the user can still interject a
   * normal prompt; the goal loop waits for that run (attachStream wires this
   * as afterDone), then this fires to put the loop back in motion. The resume
   * POST relies on the server's idempotent ensure contract — when the goal
   * should not actually run (paused/complete, or a loop already in flight
   * server-side) it is a no-op returning no run_id — so posting
   * unconditionally after any finished run is always safe.
   */
  async function continueGoalLoop(sid: string) {
    if (demo.value) return
    try {
      const { goal: g } = await api.getGoal(sid)
      goal.value[sid] = g
      if (!g || g.status !== 'active') return
      const s = sessions.value.find((x) => x.id === sid)
      const resp = await api.sendGoalCommand(sid, 'resume', s?.workspace ?? '')
      goal.value[sid] = resp.goal
      if (!resp.run_id) return
      // No admission state on the goal response: claim runId only when the
      // run actually starts, so a queued goal run cannot steal the active
      // run's interrupt/cancel slot.
      await attachStream(sid, resp.run_id, undefined, false)
    } catch {
      /* continuation is best-effort; the resume button remains available */
    }
  }

  async function send(text: string, attachments?: Attachment[], comments?: Array<{ file: string; line: number; text: string }>) {
    const sid = activeId.value
    const prompt = text.trim()
    if ((!prompt && !attachments?.length) || !sid) return
    const st = status.value[sid] ?? 'idle'
    // A run already in flight no longer blocks: the backend queues the new
    // run behind it (per-session admission). Awaiting still drops — answer
    // the interrupt first.
    if (st === 'awaiting') return

    // /goal commands execute server-side without a model turn; set/resume
    // start the goal loop and stream it like a normal run. Match "/goal"
    // followed by whitespace or end-of-input so "/goals" is NOT intercepted.
    if (/^\/goal(\s|$)/.test(prompt)) {
      await sendGoalCommand(prompt.slice('/goal'.length).trim())
      return
    }

    // Runtime agent commands (permission mode / thinking / quota) run against
    // the dedicated APIs instead of a model turn.
    if (await tryAgentCommand(prompt)) return

    push(sid, {
      id: nid(),
      kind: 'user',
      content: prompt,
      attachments: attachments?.length ? attachments : undefined,
      fresh: true,
    })
    error.value[sid] = ''
    interrupt.value[sid] = null
    streamingId.value[sid] = null

    const s = sessions.value.find((x) => x.id === sid)
    // Match every placeholder spelling: the pre-i18n zh literal (legacy
    // sessions), the server's English default title, the active locale's
    // rendering, and an empty title.
    if (s && (s.title === '\u65B0\u4F1A\u8BDD' || s.title === 'New session' || s.title === i18n.global.t('sessions.newSession') || !s.title)) {
      s.title = truncate(prompt || attachments?.[0]?.name || i18n.global.t('sessions.imageMessage'), 40)
    }

    if (demo.value) {
      push(sid, { id: nid(), kind: 'assistant', content: demoReply(prompt), streaming: false })
      status.value[sid] = 'done'
      return
    }

    // Mark the send in flight for its whole POST window: the global events
    // stream may deliver this run's `running` state before createRun
    // resolves, and maybeReattach must not treat it as a foreign run.
    sending.add(sid)
    try {
      const payload = (attachments ?? []).map((a) => ({
        name: a.name,
        mime_type: a.mimeType,
        data: a.base64,
      }))
      const res = await api.createRun(sid, prompt, activeWorkspace.value.path, payload, comments)
      // The response reports whether the run started or queued behind the
      // session gate; run_started on the stream flips queued → running. A
      // queued run leaves runId on the active run (claim only on promotion).
      status.value[sid] = res.state === 'queued' ? 'queued' : 'running'
      if (res.state === 'queued') queuePosition.value[sid] = res.queue_position
      // The status assignment above re-enables the reattach guard; drop the
      // marker before awaiting the stream so maybeReattach stays available
      // if this stream later dies mid-run.
      sending.delete(sid)
      // After an interjected user run finishes, an active goal keeps looping
      // (Codex's session-idle continuation scheduling).
      await attachStream(sid, res.run_id, () => void continueGoalLoop(sid), res.state !== 'queued')
    } catch (e) {
      sending.delete(sid)
      closeStreaming(sid)
      error.value[sid] = toErrorMessage(e)
      status.value[sid] = 'error'
    }
  }

  // tryAgentCommand handles /permissions, /thinking and /quota in the chat
  // box: the user message is echoed, the command runs against /api/agent (or
  // /api/quota), and a non-streaming assistant bubble shows the result.
  // Returns false (sending falls through to a model turn) for other prompts
  // or when no backend is connected.
  async function tryAgentCommand(prompt: string): Promise<boolean> {
    if (demo.value) return false
    const sid = activeId.value
    if (!sid) return false
    const cmd = prompt.split(/\s+/)[0]
    const arg = prompt.split(/\s+/)[1] ?? ''
    if (cmd !== '/permissions' && cmd !== '/thinking' && cmd !== '/quota') return false

    const agentCtl = useAgentStore()
    push(sid, { id: nid(), kind: 'user', content: prompt, fresh: true })
    status.value[sid] = 'running'
    error.value[sid] = ''
    let out = ''
    try {
      if (cmd === '/permissions') {
        if (arg && isPermissionMode(arg)) {
          await agentCtl.setMode(arg)
          out =
            i18n.global.t('sessions.commands.permissionSwitched', {
              label: agentCtl.modeLabel,
              mode: agentCtl.mode,
            }) + '\n'
        } else {
          await agentCtl.load()
          out =
            i18n.global.t('sessions.commands.permissionCurrent', {
              label: agentCtl.modeLabel,
              mode: agentCtl.mode,
            }) + '\n'
        }
      } else if (cmd === '/thinking') {
        if (arg && isThinkingLevel(arg)) {
          await agentCtl.setThinking(arg)
          out =
            i18n.global.t('sessions.commands.thinkingSet', {
              label: agentCtl.thinkingLabel,
              budget: agentCtl.thinkingBudget,
            }) + '\n'
        } else {
          await agentCtl.load()
          out =
            i18n.global.t('sessions.commands.thinkingCurrent', {
              label: agentCtl.thinkingLabel,
              budget: agentCtl.thinkingBudget,
            }) + '\n'
        }
      } else {
        await agentCtl.refreshQuota()
        out = agentCtl.quotaText + '\n'
      }
      push(sid, { id: nid(), kind: 'assistant', content: out, streaming: false, ts: Date.now() })
    } catch (e) {
      // Failures must also leave feedback in the chat (a banner disappears
      // when the state flips again; a chat bubble does not).
      const msg = toErrorMessage(e)
      push(sid, {
        id: nid(),
        kind: 'assistant',
        content: i18n.global.t('sessions.commands.failed', { message: msg }),
        streaming: false,
        ts: Date.now(),
      })
    }
    status.value[sid] = 'done'
    return true
  }

  // sendGoalCommand runs a /goal command against the active session and shows
  // its output in the chat; when it starts the loop, the run streams in place.
  async function sendGoalCommand(command: string) {
    const sid = activeId.value
    if (!sid) return
    const st = status.value[sid] ?? 'idle'
    if (st === 'running' || st === 'awaiting') return
    if (demo.value) return

    push(sid, { id: nid(), kind: 'user', content: `/goal ${command}`.trim(), fresh: true })
    error.value[sid] = ''
    streamingId.value[sid] = null
    try {
      const resp = await api.sendGoalCommand(sid, command, activeWorkspace.value.path)
      goal.value[sid] = resp.goal
      if (resp.output) {
        push(sid, { id: nid(), kind: 'assistant', content: resp.output, streaming: false, ts: Date.now(), srcRun: resp.run_id || undefined })
      }
      if (resp.run_id) {
        // The goal response carries no admission state: no optimistic
        // "running" — run_queued/run_started on the stream set the status,
        // and runId is claimed only when the run actually starts (a queued
        // goal run must not steal the active run's interrupt/cancel slot).
        await attachStream(sid, resp.run_id, undefined, false)
      } else {
        status.value[sid] = 'done'
      }
    } catch (e) {
      error.value[sid] = toErrorMessage(e)
      status.value[sid] = 'error'
    }
  }

  async function answerInterrupt(approve: boolean, answers?: string[]) {
    const sid = activeId.value
    const rid = runId.value[sid]
    const intr = interrupt.value[sid]
    if (!intr) return
    // Demo mode (no live run): just clear the interrupt.
    if (!rid || demo.value) {
      interrupt.value[sid] = null
      status.value[sid] = 'done'
      return
    }
    try {
      const body = intr.kind === 'ask' ? { answers: answers ?? [] } : { approve }
      await api.answerRun(rid, body)
      interrupt.value[sid] = null
      status.value[sid] = 'running'
    } catch (e) {
      error.value[sid] = toErrorMessage(e)
      status.value[sid] = 'error'
      interrupt.value[sid] = null
    }
  }

  function cancel() {
    const sid = activeId.value
    if (!sid) return
    const rid = runId.value[sid]
    if (rid) abortRun(rid)
    closeStreaming(sid)
    closeReasoning(sid)
    closeGoalNote(sid)
    interrupt.value[sid] = null
    // A run canceled with steers still pending resubmits them as a fresh
    // turn immediately (Codex parity) instead of losing them.
    const steers = pendingSteers.value[sid]
    pendingSteers.value[sid] = []
    if (!rid) {
      status.value[sid] = 'done'
      return
    }
    // Only flip to done once the server confirms the cancel; if the request
    // fails the run is still live, so surface the error instead of silently
    // pretending the run ended (the attached stream will keep updating it).
    api.cancelRun(rid).then(
      () => {
        status.value[sid] = 'done'
        if (steers && steers.length) void send(steers.join('\n'))
      },
      (e) => {
        error.value[sid] = toErrorMessage(e)
        status.value[sid] = 'error'
      },
    )
  }

  // steer feeds text into the active run's live input immediately (Codex-
  // style mid-turn steering) instead of waiting for it to finish. Silently a
  // no-op without a live run — the composer only calls this while busy with
  // status 'running'.
  function steer(text: string) {
    const sid = activeId.value
    const rid = runId.value[sid]
    const prompt = text.trim()
    if (!prompt || !sid || !rid) return
    const arr = pendingSteers.value[sid] ?? []
    arr.push(prompt)
    pendingSteers.value[sid] = arr
    void api.queueRunInput(rid, prompt).catch((e) => {
      error.value[sid] = toErrorMessage(e)
    })
  }

  return {
    sessions,
    activeId,
    activeSession,
    activeItems,
    activeStatus,
    // Raw per-session status map (exposed alongside runId/queuePosition for
    // read-only consumers and component tests).
    status,
    activeError,
    activeInterrupt,
    activeGoal,
    activePendingSteers,
    goal,
    goalRunning,
    goalPanelOpen,
    runId,
    queuePosition,
    workspaceGroups,
    historyLoading,
    activeWorkspace,
    activeWorkspaceKey,
    serverRunState,
    runStateOf,
    contextInfo,
    contextLoading,
    contextError,
    refreshContext,
    init,
    demo,
    loadSession,
    createSession,
    setActive,
    removeSession,
    send,
    sendGoalCommand,
    continueGoalLoop,
    answerInterrupt,
    cancel,
    steer,
    setDemo,
    seed,
    triggerInterrupt,
  }
})
