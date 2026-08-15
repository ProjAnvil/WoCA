import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { ChatItem, Interrupt, RunStatus, Session, TranscriptMessage } from '../types'
import * as api from '../api/client'

let seq = 0
const nid = () => `i${Date.now().toString(36)}${(seq++).toString(36)}`

function truncate(s: string, n: number): string {
  const t = s.trim()
  return t.length > n ? t.slice(0, n) + '…' : t
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
// JSON). Tool calls and results are preserved across turns this way.
function messagesToItems(msgs: TranscriptMessage[]): ChatItem[] {
  const out: ChatItem[] = []
  for (const m of msgs) {
    switch (m.role) {
      case 'human':
      case 'user':
        out.push({ id: nid(), kind: 'user', content: m.content ?? '' })
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

export const useSessionStore = defineStore('sessions', () => {
  const sessions = ref<Session[]>([])
  const activeId = ref('')

  const items = ref<Record<string, ChatItem[]>>({})
  const status = ref<Record<string, RunStatus>>({})
  const error = ref<Record<string, string>>({})
  const runId = ref<Record<string, string>>({})
  const interrupt = ref<Record<string, Interrupt | null>>({})
  const streamingId = ref<Record<string, string | null>>({})

  const activeSession = computed(() => sessions.value.find((s) => s.id === activeId.value))
  const activeItems = computed(() => items.value[activeId.value] ?? [])
  const activeStatus = computed(() => status.value[activeId.value] ?? 'idle')
  const activeError = computed(() => error.value[activeId.value] ?? '')
  const activeInterrupt = computed(() => interrupt.value[activeId.value] ?? null)

  function ensureMaps(id: string) {
    if (!items.value[id]) items.value[id] = []
    if (!status.value[id]) status.value[id] = 'idle'
    if (!interrupt.value[id]) interrupt.value[id] = null
    if (!streamingId.value[id]) streamingId.value[id] = null
  }

  // --- demo mode (the standalone demo app seeds this instead of the backend) ---
  const demo = ref(false)
  function setDemo(v: boolean) {
    demo.value = v
  }
  function seed(seedSessions: Session[], itemsMap: Record<string, ChatItem[]>) {
    sessions.value = seedSessions
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
      }))
      activeId.value = sessions.value[0].id
      await loadSession(sessions.value[0].id)
    } catch {
      await createSession()
    }
  }

  async function createSession() {
    let id = ''
    try {
      id = await api.createSession()
    } catch {
      id = `tmp-${nid()}`
    }
    sessions.value = [{ id, title: '新会话', updatedAt: '', loaded: true }, ...sessions.value]
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
    push(sid, { id, kind: 'assistant', content: '', streaming: true })
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

  async function send(text: string) {
    const sid = activeId.value
    const prompt = text.trim()
    if (!prompt || !sid) return
    const st = status.value[sid] ?? 'idle'
    if (st === 'running' || st === 'awaiting') return

    push(sid, { id: nid(), kind: 'user', content: prompt })
    status.value[sid] = 'running'
    error.value[sid] = ''
    interrupt.value[sid] = null
    streamingId.value[sid] = null

    const s = sessions.value.find((x) => x.id === sid)
    if (s && (s.title === '新会话' || !s.title)) {
      s.title = truncate(prompt, 40)
    }

    if (demo.value) {
      push(sid, { id: nid(), kind: 'assistant', content: demoReply(prompt), streaming: false })
      status.value[sid] = 'done'
      return
    }

    try {
      const { run_id } = await api.createRun(sid, prompt)
      runId.value[sid] = run_id
      await api
        .streamRun(run_id, {
          onDelta: (t) => appendDelta(sid, t),
          onToolCall: (tc) => addToolCall(sid, tc),
          onToolResult: (tr) => addToolResult(sid, tr),
          onInterrupt: (ev) => {
            interrupt.value[sid] = { kind: ev.kind, payload: ev.payload } as unknown as Interrupt
            status.value[sid] = 'awaiting'
          },
          onDone: () => {
            closeStreaming(sid)
            status.value[sid] = 'done'
          },
          onError: (msg) => {
            closeStreaming(sid)
            error.value[sid] = msg
            status.value[sid] = 'error'
          },
        })
        .catch((e) => {
          closeStreaming(sid)
          error.value[sid] = e instanceof Error ? e.message : String(e)
          status.value[sid] = 'error'
        })
    } catch (e) {
      closeStreaming(sid)
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
    init,
    createSession,
    setActive,
    send,
    answerInterrupt,
    cancel,
    setDemo,
    seed,
    triggerInterrupt,
  }
})
