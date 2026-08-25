// Transcript parsing: rebuild the UI item list from the backend's
// persisted messages.Message JSON. Pure functions — no store, no i18n —
// so the most intricate slice of session hydration is testable alone.
import type { Attachment, ChatItem, TranscriptMessage } from '../types'

let seq = 0
export const nid = () => `i${Date.now().toString(36)}${(seq++).toString(36)}`

// Rebuild the UI item list from the backend's transcript (messages.Message
// JSON). Tool calls and results are preserved across turns this way. Human
// multimodal blocks become attachment thumbnails.
//
// Goal-loop machinery renders as loop progress, not chat: <goal-cycle> human
// messages are harness-injected continuation prompts (never user speech), and
// AI text inside a goal segment is per-cycle chatter. The segment ends at the
// <goal-report>-wrapped unified reply or the next real user message.
export function messagesToItems(msgs: TranscriptMessage[]): ChatItem[] {
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
export function stripGoalReport(content: string): string {
  return content.replace(/^<goal-report>\n/, '').replace(/\n<\/goal-report>$/, '')
}

// imageAttachments extracts image content blocks from a human message.
export function imageAttachments(m: TranscriptMessage): Attachment[] | undefined {
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

