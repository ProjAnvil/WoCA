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
import { truncate, baseName } from '../lib/format'
import { useAgentStore } from './agent'

let seq = 0
const nid = () => `i${Date.now().toString(36)}${(seq++).toString(36)}`

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
        // Rebuild the thinking block from the persisted reasoning content
        // blocks so reloaded sessions show what the model thought.
        const reasoning = (m.content_blocks ?? [])
          .filter((b) => b.type === 'reasoning' && b.reasoning)
          .map((b) => b.reasoning ?? '')
          .join('')
        if (reasoning) {
          out.push({ id: nid(), kind: 'reasoning', content: reasoning, streaming: false })
        }
        if (m.content) {
          out.push({ id: nid(), kind: 'assistant', content: m.content, streaming: false })
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
      { id, title: '新会话', updatedAt: '', loaded: true, workspace: path, workspaceName: name },
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

  // removeSession deletes a session server-side and drops all of its local
  // state. Deleting the active session switches to the newest remaining
  // session of the same workspace (else the newest overall, else a fresh one).
  // A session with a live run refuses — stop it first.
  async function removeSession(id: string) {
    const st = status.value[id] ?? 'idle'
    if (st === 'running' || st === 'awaiting' || st === 'queued') {
      throw new Error('会话正在运行，请先停止再删除')
    }
    if (!demo.value) await api.deleteSession(id)
    const target = sessions.value.find((s) => s.id === id)
    const wsPath = target?.workspace ?? ''
    sessions.value = sessions.value.filter((s) => s.id !== id)
    for (const m of [items, status, queuePosition, error, runId, interrupt, streamingId, reasoningId, goal, goalRunning]) {
      delete m.value[id]
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
    // The answer's first token ends the thinking segment (it collapses).
    closeReasoning(sid)
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
  // "思考过程" header the user can expand).
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
    try {
      await api.streamRun(rid, {
        onDelta: (t) => appendDelta(sid, t),
        onReasoning: (t) => appendReasoning(sid, t),
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
          // 缺口超出服务端重放窗口（或 seq 跳变）：回源拉全量会话消息兜底。
          // loadSession 对已加载会话会跳过拉取，先重置 loaded 强制回源；
          // 重建后旧的流式气泡槽位失效，清空让后续 delta 重新开泡。
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
          status.value[sid] = 'done'
          delete queuePosition.value[sid]
          afterDone?.()
        },
        onError: (msg) => {
          closeStreaming(sid)
          closeReasoning(sid)
          error.value[sid] = msg
          status.value[sid] = 'error'
          delete queuePosition.value[sid]
        },
      })
    } catch (e) {
      closeStreaming(sid)
      closeReasoning(sid)
      error.value[sid] = e instanceof Error ? e.message : String(e)
      status.value[sid] = 'error'
      delete queuePosition.value[sid]
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
      const res = await api.createRun(sid, prompt, activeWorkspace.value.path, payload)
      // The response reports whether the run started or queued behind the
      // session gate; run_started on the stream flips queued → running. A
      // queued run leaves runId on the active run (claim only on promotion).
      status.value[sid] = res.state === 'queued' ? 'queued' : 'running'
      if (res.state === 'queued') queuePosition.value[sid] = res.queue_position
      // After an interjected user run finishes, an active goal keeps looping
      // (Codex's session-idle continuation scheduling).
      await attachStream(sid, res.run_id, () => void continueGoalLoop(sid), res.state !== 'queued')
    } catch (e) {
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
          out = `权限模式已切换为 ${agentCtl.modeLabel}（${agentCtl.mode}），后续工具调用立即生效。\n`
        } else {
          await agentCtl.load()
          out = `当前权限模式：${agentCtl.modeLabel}（${agentCtl.mode}）。\n用 /permissions <default | acceptEdits | bypassPermissions | readOnly | plan> 切换。\n`
        }
      } else if (cmd === '/thinking') {
        if (arg) {
          await agentCtl.setThinking(arg)
          out = `思考级别已设为 ${agentCtl.thinkingLabel}（${agentCtl.thinkingBudget} tokens），下一轮对话生效。\n`
        } else {
          await agentCtl.load()
          out = `当前思考级别：${agentCtl.thinkingLabel}（${agentCtl.thinkingBudget} tokens）。\n用 /thinking <off | low | medium | high> 切换。\n`
        }
      } else {
        await agentCtl.refreshQuota()
        out = agentCtl.quotaText + '\n'
      }
      push(sid, { id: nid(), kind: 'assistant', content: out, streaming: false, ts: Date.now() })
    } catch (e) {
      // 失败也要在聊天里留下反馈（横幅会随状态翻转消失，气泡不会）。
      const msg = e instanceof Error ? e.message : String(e)
      push(sid, { id: nid(), kind: 'assistant', content: `⚠ 命令执行失败：${msg}`, streaming: false, ts: Date.now() })
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
    closeStreaming(sid)
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
