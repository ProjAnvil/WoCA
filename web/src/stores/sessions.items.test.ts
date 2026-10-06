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
