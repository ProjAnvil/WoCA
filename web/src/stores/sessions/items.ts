import type { Ref } from 'vue'
import type { ChatItem } from '../../types'
import { nid } from '../../lib/transcript'

export interface ItemState {
  items: Ref<Record<string, ChatItem[]>>
  streamingId: Ref<Record<string, string | null>>
  reasoningId: Ref<Record<string, string | null>>
  goalNoteId: Ref<Record<string, string | null>>
  runId: Ref<Record<string, string>>
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
export function createItemMutations(state: ItemState) {
  const { items, streamingId, reasoningId, goalNoteId, runId } = state

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
    push(sid, { id, kind: 'reasoning', content: text, streaming: true, startedAt: Date.now() })
    reasoningId.value[sid] = id
  }

  // closeReasoning freezes the live thinking block (its card collapses to a
  // "thinking process" header the user can expand) and stamps its duration.
  function closeReasoning(sid: string) {
    const id = reasoningId.value[sid]
    reasoningId.value[sid] = null
    if (!id) return
    const ls = list(sid)
    const idx = ls.findIndex((i) => i.id === id)
    if (idx >= 0 && ls[idx].kind === 'reasoning') {
      const cur = ls[idx] as Extract<ChatItem, { kind: 'reasoning' }>
      const next = [...ls]
      next[idx] = {
        ...cur,
        streaming: false,
        durationMs:
          cur.startedAt !== undefined ? Math.max(0, Date.now() - cur.startedAt) : undefined,
      }
      items.value[sid] = next
    }
  }

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

  return {
    list,
    push,
    closeStreaming,
    ensureStreaming,
    appendDelta,
    addToolCall,
    addToolResult,
    appendReasoning,
    closeReasoning,
    appendGoalNote,
    closeGoalNote,
  }
}

export type ItemMutations = ReturnType<typeof createItemMutations>
