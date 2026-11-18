import { describe, expect, it } from 'vitest'
import {
  isNoticeContent,
  isRunStoppedNotice,
  messagesToItems,
  normalizeStopCause,
  parseRunStopped,
  stopNoticeKey,
  stripGoalReport,
} from './transcript'
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

  it('renders tagged harness notifications as dim notices, not user bubbles', () => {
    const tagged = [
      '<background-task-completed>\ntask t1 finished (done)\n</background-task-completed>',
      '<scheduled-task>\ncheck the build\n</scheduled-task>',
      '<queued-user-input>\nand this too\n</queued-user-input>',
    ]
    const items = messagesToItems([
      ...tagged.map((content) => msg({ role: 'user', content })),
      msg({ role: 'user', content: 'plain question' }),
    ])

    // One notice per tagged message — never a user bubble.
    expect(items.filter((i) => i.kind === 'notice')).toHaveLength(3)
    expect(items.filter((i) => i.kind === 'user')).toEqual([
      expect.objectContaining({ content: 'plain question' }),
    ])
    // The notice keeps the raw message content (tags included).
    expect(items.filter((i) => i.kind === 'notice').map((i) => i.content)).toEqual(tagged)
  })

  it('keeps a tagged notification with an empty body a notice', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: '<queued-user-input>\n\n</queued-user-input>' }),
    ])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ kind: 'notice', content: '<queued-user-input>\n\n</queued-user-input>' })
  })

  it('tolerates attributes on the wrapper tags (parity with the Go retain filter)', () => {
    const tagged = [
      '<background-task-completed id="t7">\ndone\n</background-task-completed>',
      '<scheduled-task id="cron-2">\ncheck the build\n</scheduled-task>',
      '<queued-user-input ts="1">\nand this too\n</queued-user-input>',
    ]
    const items = messagesToItems(tagged.map((content) => msg({ role: 'user', content })))
    expect(items.filter((i) => i.kind === 'notice')).toHaveLength(3)
    expect(items.filter((i) => i.kind === 'user')).toEqual([])
  })

  it('renders a persisted run-stopped stop notice as a notice, not a user bubble', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: 'go' }),
      msg({ role: 'user', content: '<run-stopped run="run-9" reason="user_stop"/>' }),
      msg({ role: 'user', content: 'again' }),
    ])
    // The stop tag is a machine notification: the user bubbles stay real
    // speech, the tag becomes the one notice.
    expect(items.filter((i) => i.kind === 'user').map((i) => i.content)).toEqual(['go', 'again'])
    expect(items.filter((i) => i.kind === 'notice')).toEqual([
      expect.objectContaining({ content: '<run-stopped run="run-9" reason="user_stop"/>' }),
    ])
  })

  it('recognizes the run-stopped prefix via isNoticeContent', () => {
    expect(isNoticeContent('<run-stopped run="run-9" reason="parked_ttl"/>')).toBe(true)
    expect(isNoticeContent('<run-stop')).toBe(false)
    expect(isRunStoppedNotice('<run-stopped run="run-9" reason="parked_ttl"/>')).toBe(true)
    expect(isRunStoppedNotice('<queued-user-input>\nx\n</queued-user-input>')).toBe(false)
  })

  it('folds a coalesced multi-block notification into one notice', () => {
    // CoalesceQueuedInputs joins several tagged blocks with a blank line.
    const content =
      '<queued-user-input>\ntyped while busy\n</queued-user-input>\n\n<background-task-completed>\ndone\n</background-task-completed>'
    const items = messagesToItems([msg({ role: 'user', content })])
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ kind: 'notice', content })
  })

  it('does not end a goal segment: chatter after a notification stays goal narration', () => {
    const items = messagesToItems([
      msg({ role: 'user', content: '<goal-cycle n=1>' }),
      msg({ role: 'user', content: '<background-task-completed>\ndone\n</background-task-completed>' }),
      msg({ role: 'ai', content: 'cycle chatter' }),
      msg({ role: 'ai', content: '<goal-report>\nfinal\n</goal-report>' }),
    ])
    expect(items.find((i) => i.kind === 'notice')).toMatchObject({
      content: '<background-task-completed>\ndone\n</background-task-completed>',
    })
    expect(items.find((i) => i.kind === 'goal_note')).toMatchObject({ content: 'cycle chatter' })
    expect(items.find((i) => i.kind === 'assistant')).toMatchObject({ content: 'final' })
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

describe('parseRunStopped', () => {
  it('extracts the run id and every wire-stable stop cause', () => {
    expect(parseRunStopped('<run-stopped run="run-9" reason="user_stop"/>')).toEqual({
      run: 'run-9',
      reason: 'user_stop',
    })
    expect(parseRunStopped('<run-stopped run="r1" reason="parked_ttl"/>')).toEqual({
      run: 'r1',
      reason: 'parked_ttl',
    })
    expect(parseRunStopped('<run-stopped run="r1" reason="server_stop"/>')).toEqual({
      run: 'r1',
      reason: 'server_stop',
    })
    expect(parseRunStopped('<run-stopped run="r1" reason="lease_lost"/>')).toEqual({
      run: 'r1',
      reason: 'lease_lost',
    })
  })

  it('treats a malformed or missing reason attribute as a user stop', () => {
    expect(parseRunStopped('<run-stopped run="r1"/>').reason).toBe('user_stop')
    expect(parseRunStopped('<run-stopped reason="garbled"/>')).toEqual({ run: '', reason: 'user_stop' })
    expect(parseRunStopped('<run-stopped/>').reason).toBe('user_stop')
  })
})

// The cause set is the web twin of the Go constants in
// internal/server/stopmark.go (see internal/machinetag for the tag prefix);
// unknown causes must degrade to the user-stop copy, never render raw.
describe('stop cause normalization and copy routing', () => {
  it('normalizes every known cause and degrades unknowns to user_stop', () => {
    expect(normalizeStopCause('user_stop')).toBe('user_stop')
    expect(normalizeStopCause('parked_ttl')).toBe('parked_ttl')
    expect(normalizeStopCause('server_stop')).toBe('server_stop')
    expect(normalizeStopCause('lease_lost')).toBe('lease_lost')
    expect(normalizeStopCause('garbled')).toBe('user_stop')
    expect(normalizeStopCause(undefined)).toBe('user_stop')
    expect(normalizeStopCause(42)).toBe('user_stop')
  })

  it('routes each cause to its chat message key, unknowns to the stop copy', () => {
    expect(stopNoticeKey('user_stop')).toBe('chat.generationStopped')
    expect(stopNoticeKey('parked_ttl')).toBe('chat.runTimedOut')
    expect(stopNoticeKey('server_stop')).toBe('chat.runServerStopped')
    expect(stopNoticeKey('lease_lost')).toBe('chat.runLeaseLost')
    expect(stopNoticeKey(undefined)).toBe('chat.generationStopped')
    expect(stopNoticeKey('garbled')).toBe('chat.generationStopped')
  })
})
