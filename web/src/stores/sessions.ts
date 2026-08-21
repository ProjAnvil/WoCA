import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type {
  Attachment,
  ChatItem,
  ContextInfo,
  Goal,
  Interrupt,
  RunStatus,
  Session,
  TranscriptMessage,
  WorkspaceGroup,
} from '../types'
import * as api from '../api/client'
import i18n from '../i18n'
import { truncate, baseName } from '../lib/format'
import { useAgentStore } from './agent'

let seq = 0
const nid = () => `i${Date.now().toString(36)}${(seq++).toString(36)}`

function isAbortError(value: unknown): boolean {
  return typeof value === 'object' && value !== null && 'name' in value && value.name === 'AbortError'
}

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

// Rebuild the UI item list from the backend's transcript (messages.Message
// JSON). Tool calls and results are preserved across turns this way. Human
// multimodal blocks become attachment thumbnails.
//
// Goal-loop machinery renders as loop progress, not chat: <goal-cycle> human
// messages are harness-injected continuation prompts (never user speech), and
// AI text inside a goal segment is per-cycle chatter. The segment ends at the
// <goal-report>-wrapped unified reply or the next real user message.
function messagesToItems(msgs: TranscriptMessage[]): ChatItem[] {
  // Tool results land on their call's card (input + output in one card).
  // First-wins: a call id appears once per transcript.
  const results = new Map<string, string>()
  for (const m of msgs) {
    if (m.role === 'tool' && m.tool_call_id && !results.has(m.tool_call_id)) {
      results.set(m.tool_call_id, m.content ?? '')
    }
  }
  const out: ChatItem[] = []
  const consumed = new Set<string>()
  let inGoal = false
  for (const m of msgs) {
    switch (m.role) {
      case 'human':
      case 'user': {
        const content = m.content ?? ''
        if (content.startsWith('<goal-cycle')) {
          inGoal = true
          break // harness-injected goal continuation prompt — not user speech
        }
        if (content.startsWith('<relevant-memory')) {
          // ladyM recall context injected after tool results — model input,
          // never user speech (same class of harness message as goal-cycle).
          break
        }
        inGoal = false
        out.push({
          id: nid(),
          kind: 'user',
          content,
          attachments: imageAttachments(m),
        })
        break
      }
      case 'ai':
      case 'assistant':
        // Rebuild the thinking block from the persisted reasoning content
        // blocks so reloaded sessions show what the model thought.
        const reasoning = (m.content_blocks ?? [])
          .filter((b) => b.type === 'reasoning' && b.reasoning)
          .map((b) => b.reasoning ?? '')
          .join('')
        if (reasoning) {
          out.push({ id: nid(), kind: 'reasoning', content: reasoning, streaming: false })
        }
        const content = m.content ?? ''
        if (inGoal && content.startsWith('<goal-report>')) {
          // The run's single unified reply: the one goal text shown as an
          // assistant bubble, with the persistence markers stripped.
          out.push({ id: nid(), kind: 'assistant', content: stripGoalReport(content), streaming: false })
        } else if (inGoal) {
          if (content) {
            out.push({ id: nid(), kind: 'goal_note', content, streaming: false })
          }
        } else if (content) {
          out.push({ id: nid(), kind: 'assistant', content, streaming: false })
        }
        for (const tc of m.tool_calls ?? []) {
          const call: Extract<ChatItem, { kind: 'tool_call' }> = {
            id: nid(),
            kind: 'tool_call',
            toolCallId: tc.id ?? '',
            name: tc.name,
            args: tc.args ?? {},
          }
          const res = results.get(tc.id ?? '')
          if (res !== undefined) {
            call.result = res
            consumed.add(tc.id ?? '')
          }
          out.push(call)
        }
        break
      case 'tool':
        // Merged above when its call is in the transcript; orphan results
        // (call trimmed away) keep a standalone card.
        if (!consumed.has(m.tool_call_id ?? '')) {
          out.push({
            id: nid(),
            kind: 'tool_result',
            toolCallId: m.tool_call_id ?? '',
            content: m.content ?? '',
          })
        }
        break
    }
  }
  return out
}

// stripGoalReport removes the <goal-report> persistence markers the backend
// wraps the goal run's unified reply with (they identify it on reload).
function stripGoalReport(content: string): string {
  return content.replace(/^<goal-report>\n/, '').replace(/\n<\/goal-report>$/, '')
}

// imageAttachments extracts image content blocks from a human message.
function imageAttachments(m: TranscriptMessage): Attachment[] | undefined {
  const imgs = (m.content_blocks ?? []).filter((b) => b.type === 'image' && b.base64)
  if (!imgs.length) return undefined
  return imgs.map((b, i) => ({
    id: nid() + i,
    name: `image-${i + 1}`,
    mimeType: b.mime_type ?? 'image/png',
    dataUrl: `data:${b.mime_type ?? 'image/png'};base64,${b.base64}`,
    base64: b.base64 ?? '',
  }))
}

export const useSessionStore = defineStore('sessions', () => {
  const sessions = ref<Session[]>([])
  const activeId = ref('')

  const items = ref<Record<string, ChatItem[]>>({})
  const status = ref<Record<string, RunStatus>>({})
  // 1-based queue position while a run waits on the session gate (backend
  // admission; absent = not queued).
  const queuePosition = ref<Record<string, number>>({})
  const error = ref<Record<string, string>>({})
  const runId = ref<Record<string, string>>({})
  const interrupt = ref<Record<string, Interrupt | null>>({})
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
  // Context-usage snapshot for the chat input's context indicator. Hover
  // refreshes it on demand; a run finishing refreshes it in the background so
  // the always-visible chip number stays current.
  const contextInfo = ref<ContextInfo | null>(null)
  const contextLoading = ref(false)
  const contextError = ref('')

  // Server-truth run state per session (running / awaiting), seeded from the
  // session list and kept live by the global /api/events stream. This is the
  // authoritative signal for the sidebar badge — it survives page refreshes
  // and reflects runs other tabs started, unlike the local status map which
  // only covers streams this tab attached.
  const serverRunState = ref<Record<string, { status: 'running' | 'awaiting'; runId: string }>>({})
  let eventsSource: api.EventStream | null = null
  const runStreamControllers = new Map<string, AbortController>()
  const reattaching = new Set<string>()
  // Sessions with a createRun POST in flight. The server's global `running`
  // event races the POST response; without this marker maybeReattach would
  // treat our own just-started run as a foreign one (local status is still
  // the previous turn's) and rebuild items from the pre-run disk snapshot,
  // wiping the optimistic user bubble send() just pushed.
  const sending = new Set<string>()

  const activeSession = computed(() => sessions.value.find((s) => s.id === activeId.value))
  const activeItems = computed(() => items.value[activeId.value] ?? [])
  const activeStatus = computed(() => status.value[activeId.value] ?? 'idle')
  const activeError = computed(() => error.value[activeId.value] ?? '')
  const activeInterrupt = computed(() => interrupt.value[activeId.value] ?? null)
  const activeGoal = computed(() => goal.value[activeId.value] ?? null)

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

  // --- server-truth run state (sidebar badge + refresh reattach) ---

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

  // --- demo mode (the standalone demo app seeds this instead of the backend) ---
  const demo = ref(false)
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
      try {
        const { messages } = await api.getSession(id)
        items.value[id] = messagesToItems(messages)
        s.loaded = true
      } catch {
        items.value[id] = []
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

  // refreshContext reloads the active session's context-usage snapshot (the
  // hover popover's payload and the chip number). An empty session (nothing
  // sent yet) has no usage to report — the chip stays disabled until the
  // first message lands. Local-only sessions (demo mode / failed create)
  // have no server-side transcript either, so they keep the last snapshot.
  async function refreshContext() {
    const sid = activeId.value
    if (!sid || sid.startsWith('tmp-') || demo.value) return
    if ((items.value[sid] ?? []).length === 0) {
      contextInfo.value = null
      return
    }
    contextLoading.value = true
    contextError.value = ''
    try {
      contextInfo.value = await api.getSessionContext(sid)
    } catch (e) {
      contextError.value = e instanceof Error ? e.message : String(e)
    } finally {
      contextLoading.value = false
    }
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

  // --- item mutations ---

  function list(sid: string): ChatItem[] {
    return items.value[sid] ?? []
  }

  function push(sid: string, item: ChatItem) {
    items.value[sid] = [...list(sid), item]
  }

  function closeStreaming(sid: string) {
    const id = streamingId.value[sid]
    streamingId.value[sid] = null
    if (!id) return
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx >= 0 && ls[idx].kind === 'assistant') {
      const next = [...ls]
      next[idx] = { ...(next[idx] as Extract<ChatItem, { kind: 'assistant' }>), streaming: false }
      items.value[sid] = next
    }
  }

  function ensureStreaming(sid: string): string {
    const existing = streamingId.value[sid]
    if (existing) return existing
    const id = nid()
    push(sid, { id, kind: 'assistant', content: '', streaming: true, ts: Date.now(), srcRun: runId.value[sid] })
    streamingId.value[sid] = id
    return id
  }

  /*
   * Live-block lifecycle: while a run streams, a session holds at most two
   * "live" items — the reasoning (thinking) block tracked by reasoningId and
   * the assistant answer tracked by streamingId. The answer bubble is created
   * lazily by ensureStreaming on the first delta, not when the run starts, so
   * tool-only turns never render an empty bubble. Both blocks are frozen
   * (streaming: false) by closeStreaming/closeReasoning on done/error.
   * appendDelta and addToolCall also close the reasoning block: the first
   * answer token or any tool call means the model has stopped thinking, so
   * the card must collapse even though the run continues.
   */
  function appendDelta(sid: string, text: string) {
    if (!text) return
    // The answer's first token ends the thinking segment (it collapses) and
    // the goal-loop narration block (the unified reply is arriving).
    closeReasoning(sid)
    closeGoalNote(sid)
    const id = ensureStreaming(sid)
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx < 0) return
    const cur = ls[idx] as Extract<ChatItem, { kind: 'assistant' }>
    const next = [...ls]
    next[idx] = { ...cur, content: cur.content + text }
    items.value[sid] = next
  }

  // A tool call also ends the live thinking segment.
  function addToolCall(sid: string, tc: { id: string; name: string; args: Record<string, unknown> }) {
    closeStreaming(sid)
    closeReasoning(sid)
    push(sid, { id: nid(), kind: 'tool_call', toolCallId: tc.id, name: tc.name, args: tc.args })
  }

  function addToolResult(sid: string, tr: { tool_call_id: string; content: string }) {
    // The result lands on its call's card (input + output in one card). An
    // orphan result (no matching call in the list) falls back to a standalone
    // card so it is never silently dropped.
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.kind === 'tool_call' && i.toolCallId === tr.tool_call_id)
    if (idx >= 0) {
      const cur = ls[idx] as Extract<ChatItem, { kind: 'tool_call' }>
      const next = [...ls]
      next[idx] = { ...cur, result: tr.content }
      items.value[sid] = next
      return
    }
    push(sid, { id: nid(), kind: 'tool_result', toolCallId: tr.tool_call_id, content: tr.content })
  }

  // --- reasoning (thinking) items ---

  // appendReasoning extends the session's live thinking block, creating it
  // before the assistant bubble on first fragment.
  function appendReasoning(sid: string, text: string) {
    if (!text) return
    let id = reasoningId.value[sid]
    const ls = list(sid)
    const idx = id ? ls.findIndex((i) => i.id === id) : -1
    if (idx >= 0 && ls[idx].kind === 'reasoning') {
      const cur = ls[idx] as Extract<ChatItem, { kind: 'reasoning' }>
      const next = [...ls]
      next[idx] = { ...cur, content: cur.content + text }
      items.value[sid] = next
      return
    }
    id = nid()
    push(sid, { id, kind: 'reasoning', content: text, streaming: true })
    reasoningId.value[sid] = id
  }

  // closeReasoning freezes the live thinking block (its card collapses to a
  // "thinking process" header the user can expand).
  function closeReasoning(sid: string) {
    const id = reasoningId.value[sid]
    reasoningId.value[sid] = null
    if (!id) return
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx >= 0 && ls[idx].kind === 'reasoning') {
      const next = [...ls]
      next[idx] = { ...(next[idx] as Extract<ChatItem, { kind: 'reasoning' }>), streaming: false }
      items.value[sid] = next
    }
  }

  // --- goal-loop narration (progress, never an answer bubble) ---

  // appendGoalNote extends the session's live goal-loop narration block —
  // cycle banners and the executor's per-cycle chatter — created lazily on
  // the first fragment of a goal run.
  function appendGoalNote(sid: string, text: string) {
    if (!text) return
    let id = goalNoteId.value[sid]
    const ls = list(sid)
    const idx = id ? ls.findIndex((i) => i.id === id) : -1
    if (idx >= 0 && ls[idx].kind === 'goal_note') {
      const cur = ls[idx] as Extract<ChatItem, { kind: 'goal_note' }>
      const next = [...ls]
      next[idx] = { ...cur, content: cur.content + text }
      items.value[sid] = next
      return
    }
    id = nid()
    push(sid, { id, kind: 'goal_note', content: text, streaming: true })
    goalNoteId.value[sid] = id
  }

  // closeGoalNote freezes the live narration block; the unified reply or the
  // run's terminal event unfreezes it.
  function closeGoalNote(sid: string) {
    const id = goalNoteId.value[sid]
    goalNoteId.value[sid] = null
    if (!id) return
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx >= 0 && ls[idx].kind === 'goal_note') {
      const next = [...ls]
      next[idx] = { ...(next[idx] as Extract<ChatItem, { kind: 'goal_note' }>), streaming: false }
      items.value[sid] = next
    }
  }

  // --- run lifecycle ---

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
      error.value[sid] = e instanceof Error ? e.message : String(e)
      status.value[sid] = 'error'
      delete queuePosition.value[sid]
    } finally {
      if (runStreamControllers.get(rid) === controller) runStreamControllers.delete(rid)
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

  async function send(text: string, attachments?: Attachment[]) {
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
    })
    error.value[sid] = ''
    interrupt.value[sid] = null
    streamingId.value[sid] = null

    const s = sessions.value.find((x) => x.id === sid)
    // The zh literal also matches sessions created before the i18n switch or
    // under the other locale, so the default title is still replaced.
    if (s && (s.title === '新会话' || s.title === i18n.global.t('sessions.newSession') || !s.title)) {
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
      const res = await api.createRun(sid, prompt, activeWorkspace.value.path, payload)
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
      error.value[sid] = e instanceof Error ? e.message : String(e)
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
    push(sid, { id: nid(), kind: 'user', content: prompt })
    status.value[sid] = 'running'
    error.value[sid] = ''
    let out = ''
    try {
      if (cmd === '/permissions') {
        if (arg) {
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
        if (arg) {
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
      const msg = e instanceof Error ? e.message : String(e)
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

    push(sid, { id: nid(), kind: 'user', content: `/goal ${command}`.trim() })
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
      error.value[sid] = e instanceof Error ? e.message : String(e)
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
      error.value[sid] = e instanceof Error ? e.message : String(e)
      status.value[sid] = 'error'
      interrupt.value[sid] = null
    }
  }

  function cancel() {
    const sid = activeId.value
    if (!sid) return
    const rid = runId.value[sid]
    if (rid) runStreamControllers.get(rid)?.abort()
    closeStreaming(sid)
    closeReasoning(sid)
    closeGoalNote(sid)
    interrupt.value[sid] = null
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
      },
      (e) => {
        error.value[sid] = e instanceof Error ? e.message : String(e)
        status.value[sid] = 'error'
      },
    )
  }

  return {
    sessions,
    activeId,
    activeSession,
    activeItems,
    activeStatus,
    activeError,
    activeInterrupt,
    activeGoal,
    goal,
    goalRunning,
    goalPanelOpen,
    runId,
    queuePosition,
    workspaceGroups,
    activeWorkspace,
    activeWorkspaceKey,
    serverRunState,
    runStateOf,
    contextInfo,
    contextLoading,
    contextError,
    refreshContext,
    init,
    createSession,
    setActive,
    removeSession,
    send,
    sendGoalCommand,
    continueGoalLoop,
    answerInterrupt,
    cancel,
    setDemo,
    seed,
    triggerInterrupt,
  }
})
