import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { createItemMutations } from './sessions/items'
import type { ChatItem } from '../types'

function setup() {
  const items = ref<Record<string, ChatItem[]>>({})
  return createItemMutations({
    items,
    streamingId: ref<Record<string, string | null>>({}),
    reasoningId: ref<Record<string, string | null>>({}),
    goalNoteId: ref<Record<string, string | null>>({}),
    runId: ref<Record<string, string>>({}),
  })
}

describe('reasoning duration', () => {
  it('records startedAt on creation and durationMs on close', () => {
    const now = 1_700_000_000_000
    let t = now
    vi.spyOn(Date, 'now').mockImplementation(() => t)
    const m = setup()
    m.appendReasoning('s1', 'think')
    m.closeReasoning('s1')
    const r = m.list('s1')[0] as Extract<ChatItem, { kind: 'reasoning' }>
    expect(r.startedAt).toBe(now)
    m.appendReasoning('s1', 'more')
    t = now + 2500 // advance between append and close so the delta path is real
    m.closeReasoning('s1')
    const r2 = m.list('s1')[1] as Extract<ChatItem, { kind: 'reasoning' }>
    expect(r2.durationMs).toBe(2500)
    expect(r2.streaming).toBe(false)
    vi.restoreAllMocks()
  })
})

describe('tool card dual-channel dedup (goal runs)', () => {
  // The goal graph delivers every execute-cycle message on TWO channels —
  // the GoalDelta custom chunk and the outer node's updates batch — so the
  // SSE stream legitimately shows the same tool_call twice. The live card
  // list must keep ONE card per call id: a second card would never hear
  // its result (the result lands on the first card) and spins forever.
  it('a re-delivered tool_call opens no second card; the result closes the one card', () => {
    const m = setup()
    const call = { id: 'g1', name: 'Bash', args: {} }
    m.addToolCall('s1', call)
    m.addToolCall('s1', call) // the dual channel's second copy
    expect(m.list('s1')).toHaveLength(1)

    m.addToolResult('s1', { tool_call_id: 'g1', content: 'ok' })
    const cards = m.list('s1').filter((i) => i.kind === 'tool_call')
    expect(cards).toHaveLength(1)
    expect((cards[0] as Extract<ChatItem, { kind: 'tool_call' }>).result).toBe('ok')
  })

  it('distinct calls keep distinct cards, each closed by its own result', () => {
    const m = setup()
    m.addToolCall('s1', { id: 'c1', name: 'Bash', args: {} })
    m.addToolCall('s1', { id: 'c2', name: 'Read', args: {} })
    expect(m.list('s1')).toHaveLength(2)
    m.addToolResult('s1', { tool_call_id: 'c2', content: 'file' })
    m.addToolResult('s1', { tool_call_id: 'c1', content: 'done' })
    const byId = Object.fromEntries(
      m
        .list('s1')
        .filter((i) => i.kind === 'tool_call')
        .map((i) => [(i as Extract<ChatItem, { kind: 'tool_call' }>).toolCallId, i]),
    )
    expect((byId.c1 as Extract<ChatItem, { kind: 'tool_call' }>).result).toBe('done')
    expect((byId.c2 as Extract<ChatItem, { kind: 'tool_call' }>).result).toBe('file')
  })

  it('a replayed result overwrites the same card in place (no duplicate output cards)', () => {
    const m = setup()
    m.addToolCall('s1', { id: 'g1', name: 'Bash', args: {} })
    m.addToolResult('s1', { tool_call_id: 'g1', content: 'once' })
    m.addToolResult('s1', { tool_call_id: 'g1', content: 'once' }) // dual-channel replay
    expect(m.list('s1')).toHaveLength(1)
    expect((m.list('s1')[0] as Extract<ChatItem, { kind: 'tool_call' }>).result).toBe('once')
  })
})
