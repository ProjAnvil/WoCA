import { describe, expect, it } from 'vitest'
import { messagesToItems, stripGoalReport } from './transcript'
import type { TranscriptMessage } from '../types'

function msg(over: Partial<TranscriptMessage>): TranscriptMessage {
  return { role: 'ai', content: '', ...over } as TranscriptMessage
}

describe('messagesToItems', () => {
  it('merges tool results onto their call card (input + output together)', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: 'list the files' }),
      msg({
        role: 'ai',
        tool_calls: [{ id: 'c1', name: 'Bash', args: { command: 'ls' } }],
      }),
      msg({ role: 'tool', tool_call_id: 'c1', content: 'a.txt\nb.txt' }),
    ])

    expect(items).toHaveLength(2)
    const call = items.find((i) => i.kind === 'tool_call')
    expect(call).toMatchObject({ name: 'Bash', result: 'a.txt\nb.txt' })
  })

  it('keeps an orphan tool result as a standalone card when its call was trimmed away', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: 'go' }),
      msg({ role: 'tool', tool_call_id: 'gone', content: 'late output' }),
    ])
    expect(items).toHaveLength(2)
    expect(items[1]).toMatchObject({ kind: 'tool_result', content: 'late output' })
  })

  it('renders goal-loop machinery as progress, not chat', () => {
    const report = '<goal-report>\nfinal answer\n</goal-report>'
    const items = messagesToItems([
      msg({ role: 'user', content: '<goal-cycle n=1>' }),
      msg({ role: 'ai', content: 'cycle chatter' }),
      msg({ role: 'user', content: '<goal-cycle n=2>' }),
      msg({ role: 'ai', content: report }),
    ])

    // No user bubbles at all: goal-cycle prompts are harness-injected.
    expect(items.filter((i) => i.kind === 'user')).toEqual([])
    expect(items.find((i) => i.kind === 'goal_note')).toMatchObject({ content: 'cycle chatter' })
    // The unified reply keeps its text but loses the persistence markers.
    expect(items.find((i) => i.kind === 'assistant')).toMatchObject({ content: 'final answer' })
  })

  it('filters ladyM recall context — model input, never user speech', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: '<relevant-memory id="m1">facts</relevant-memory>' }),
      msg({ role: 'user', content: 'real question' }),
      msg({ role: 'ai', content: 'answer' }),
    ])
    expect(items.filter((i) => i.kind === 'user')).toHaveLength(1)
    expect(items[0]).toMatchObject({ content: 'real question' })
  })

  it('rebuilds the thinking block from persisted reasoning content blocks', () => {
    const items = messagesToItems([
      msg({
        role: 'ai',
        content: 'the answer',
        content_blocks: [
          { type: 'reasoning', reasoning: 'step one. ' },
          { type: 'reasoning', reasoning: 'step two.' },
          { type: 'text', text: 'ignored' },
        ],
      }),
    ])
    expect(items[0]).toMatchObject({ kind: 'reasoning', content: 'step one. step two.' })
    expect(items[1]).toMatchObject({ kind: 'assistant', content: 'the answer' })
  })

  it('extracts image attachments from human multimodal blocks', () => {
    const items = messagesToItems([
      msg({
        role: 'user',
        content: 'look',
        content_blocks: [
          { type: 'image', mime_type: 'image/png', base64: 'AAAA' },
          { type: 'image', mime_type: 'image/jpeg', base64: 'BBBB' },
        ],
      }),
    ])
    expect(items[0]).toMatchObject({ kind: 'user' })
    const atts = (items[0] as { attachments?: unknown[] }).attachments
    expect(atts).toHaveLength(2)
    expect((atts as { dataUrl: string }[])[0].dataUrl).toBe('data:image/png;base64,AAAA')
  })
})

describe('stripGoalReport', () => {
  it('removes exactly the wrapping markers', () => {
    expect(stripGoalReport('<goal-report>\nbody\n</goal-report>')).toBe('body')
    expect(stripGoalReport('no markers')).toBe('no markers')
  })
})
