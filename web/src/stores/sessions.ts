import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type {
  Attachment,
  ChatItem,
  Goal,
  Interrupt,
  RunStatus,
  Session,
  TranscriptMessage,
  WorkspaceGroup,
} from '../types'
import * as api from '../api/client'

let seq = 0
const nid = () => `i${Date.now().toString(36)}${(seq++).toString(36)}`

function truncate(s: string, n: number): string {
  const t = s.trim()
  return t.length > n ? t.slice(0, n) + '…' : t
}

function baseName(path: string): string {
  const p = path.replace(/\/+$/, '')
  const i = p.lastIndexOf('/')
  return i >= 0 ? p.slice(i + 1) : p
}

// demoReply is a canned assistant answer used when the store runs in demo mode
// (no backend), so the standalone demo remains interactive.
function demoReply(prompt: string): string {
  return [
    `（演示模式）这是对「${truncate(prompt, 24)}」的示例回复，当前未连接后端。`,
    '',
    '```',
    '这里会显示流式 token、工具调用与结果。',
    '```',
    '',
    '- 真实模式通过 `POST /api/runs` + SSE 流式返回',
    '- 工具调用与审批会以卡片/弹窗呈现',
  ].join('\n')
}

// Rebuild the UI item list from the backend's transcript (messages.Message
// JSON). Tool calls and results are preserved across turns this way. Human
// multimodal blocks become attachment thumbnails.
function messagesToItems(msgs: TranscriptMessage[]): ChatItem[] {
  const out: ChatItem[] = []
  for (const m of msgs) {
    switch (m.role) {
      case 'human':
      case 'user':
        out.push({
          id: nid(),
          kind: 'user',
          content: m.content ?? '',
          attachments: imageAttachments(m),
        })
        break
      case 'ai':
      case 'assistant':
        if (m.content) {
          out.push({ id: nid(), kind: 'assistant', content: m.content, streaming: false })
        }
        for (const tc of m.tool_calls ?? []) {
          out.push({
            id: nid(),
            kind: 'tool_call',
            toolCallId: tc.id ?? '',
            name: tc.name,
            args: tc.args ?? {},
          })
        }
        break
      case 'tool':
        out.push({
          id: nid(),
          kind: 'tool_result',
          toolCallId: m.tool_call_id ?? '',
          content: m.content ?? '',
        })
        break
    }
  }
  return out
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
  const error = ref<Record<string, string>>({})
  const runId = ref<Record<string, string>>({})
  const interrupt = ref<Record<string, Interrupt | null>>({})
  const streamingId = ref<Record<string, string | null>>({})
  // Goal mode: per-session goal snapshot + whether the goal loop is running.
  const goal = ref<Record<string, Goal | null>>({})
  const goalRunning = ref<Record<string, boolean>>({})
  const goalPanelOpen = ref(false)

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
      const name = s.workspaceName || (path ? baseName(path) : '') || '默认工作区'
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
      return { path: s.workspace ?? '', name: s.workspaceName || (s.workspace ? baseName(s.workspace) : '') || '默认工作区' }
    }
    const g = workspaceGroups.value[0]
    return g ? { path: g.path, name: g.name } : { path: '', name: '默认工作区' }
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
        workspaceName: s.workspace_name ?? (s.workspace ? baseName(s.workspace) : '默认工作区'),
      }))
      activeId.value = sessions.value[0].id
      await loadSession(sessions.value[0].id)
    } catch {
      await createSession()
    }
  }

  async function createSession(workspace?: { path?: string; name?: string }) {
    const wsPath = workspace?.path ?? activeWorkspace.value.path
    const wsName = workspace?.name ?? activeWorkspace.value.name
    let id = ''
    try {
      const resp = await api.createSession(wsPath)
      id = resp.session_id
    } catch {
      id = `tmp-${nid()}`
    }
    sessions.value = [
      { id, title: '新会话', updatedAt: '', loaded: true, workspace: wsPath, workspaceName: wsName },
      ...sessions.value,
    ]
    ensureMaps(id)
    activeId.value = id
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
  }

  async function setActive(id: string) {
    activeId.value = id
    await loadSession(id)
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

  function appendDelta(sid: string, text: string) {
    if (!text) return
    const id = ensureStreaming(sid)
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx < 0) return
    const cur = ls[idx] as Extract<ChatItem, { kind: 'assistant' }>
    const next = [...ls]
    next[idx] = { ...cur, content: cur.content + text }
    items.value[sid] = next
  }

  function addToolCall(sid: string, tc: { id: string; name: string; args: Record<string, unknown> }) {
    closeStreaming(sid)
    push(sid, { id: nid(), kind: 'tool_call', toolCallId: tc.id, name: tc.name, args: tc.args })
  }

  function addToolResult(sid: string, tr: { tool_call_id: string; content: string }) {
    push(sid, { id: nid(), kind: 'tool_result', toolCallId: tr.tool_call_id, content: tr.content })
  }

  // --- run lifecycle ---

  // attachStream subscribes to a run's SSE stream, updating this session's
  // chat items and goal chip. afterDone runs once the stream ends (used for
  // the goal loop's idle auto-continue after an interjected user run).
  async function attachStream(
    sid: string,
    rid: string,
    afterDone?: () => void,
  ) {
    runId.value[sid] = rid
    try {
      await api.streamRun(rid, {
        onDelta: (t) => appendDelta(sid, t),
        onToolCall: (tc) => addToolCall(sid, tc),
        onToolResult: (tr) => addToolResult(sid, tr),
        onInterrupt: (ev) => {
          interrupt.value[sid] = { kind: ev.kind, payload: ev.payload } as unknown as Interrupt
          status.value[sid] = 'awaiting'
        },
        onGoal: (ev) => {
          goal.value[sid] = ev.goal
          goalRunning.value[sid] = ev.running
        },
        onDone: () => {
          closeStreaming(sid)
          status.value[sid] = 'done'
          afterDone?.()
        },
        onError: (msg) => {
          closeStreaming(sid)
          error.value[sid] = msg
          status.value[sid] = 'error'
        },
      })
    } catch (e) {
      closeStreaming(sid)
      error.value[sid] = e instanceof Error ? e.message : String(e)
      status.value[sid] = 'error'
    }
  }

  // continueGoalLoop implements idle-detection continuation: when a session
  // holds an active (non-running) goal, ask the server to keep looping — the
  // ensure-semantics POST is a no-op unless a run should start.
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
      status.value[sid] = 'running'
      await attachStream(sid, resp.run_id)
    } catch {
      /* continuation is best-effort; the resume button remains available */
    }
  }

  async function send(text: string, attachments?: Attachment[]) {
    const sid = activeId.value
    const prompt = text.trim()
    if ((!prompt && !attachments?.length) || !sid) return
    const st = status.value[sid] ?? 'idle'
    if (st === 'running' || st === 'awaiting') return

    // /goal commands execute server-side without a model turn; set/resume
    // start the goal loop and stream it like a normal run.
    if (prompt.startsWith('/goal')) {
      await sendGoalCommand(prompt.slice('/goal'.length).trim())
      return
    }

    push(sid, {
      id: nid(),
      kind: 'user',
      content: prompt,
      attachments: attachments?.length ? attachments : undefined,
    })
    status.value[sid] = 'running'
    error.value[sid] = ''
    interrupt.value[sid] = null
    streamingId.value[sid] = null

    const s = sessions.value.find((x) => x.id === sid)
    if (s && (s.title === '新会话' || !s.title)) {
      s.title = truncate(prompt || attachments?.[0]?.name || '图片消息', 40)
    }

    if (demo.value) {
      push(sid, { id: nid(), kind: 'assistant', content: demoReply(prompt), streaming: false })
      status.value[sid] = 'done'
      return
    }

    try {
      const payload = (attachments ?? []).map((a) => ({
        name: a.name,
        mime_type: a.mimeType,
        data: a.base64,
      }))
      const { run_id } = await api.createRun(sid, prompt, activeWorkspace.value.path, payload)
      // After an interjected user run finishes, an active goal keeps looping
      // (Codex's session-idle continuation scheduling).
      await attachStream(sid, run_id, () => void continueGoalLoop(sid))
    } catch (e) {
      closeStreaming(sid)
      error.value[sid] = e instanceof Error ? e.message : String(e)
      status.value[sid] = 'error'
    }
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
    status.value[sid] = 'running'
    error.value[sid] = ''
    streamingId.value[sid] = null
    try {
      const resp = await api.sendGoalCommand(sid, command, activeWorkspace.value.path)
      goal.value[sid] = resp.goal
      if (resp.output) {
        push(sid, { id: nid(), kind: 'assistant', content: resp.output, streaming: false, ts: Date.now(), srcRun: resp.run_id || undefined })
      }
      if (resp.run_id) {
        await attachStream(sid, resp.run_id)
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
    const rid = runId.value[sid]
    if (rid) api.cancelRun(rid).catch(() => {})
    closeStreaming(sid)
    interrupt.value[sid] = null
    status.value[sid] = 'done'
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
    workspaceGroups,
    activeWorkspace,
    activeWorkspaceKey,
    init,
    createSession,
    setActive,
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
