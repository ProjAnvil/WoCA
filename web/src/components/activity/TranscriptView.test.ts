// Task 12: TranscriptView — the readonly child transcript surface: the
// finished child's report as a role="log" mono block (max-h 200px scroll)
// with a copy button; the live placeholder while the child runs. The v1
// source decision: the registry's Report field (no child-session event
// stream exists) — documented in the task report.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import TranscriptView from './TranscriptView.vue'
import type { SubagentRun } from '../../types'

vi.mock('../../api/client', () => ({}))

const run = (over: Partial<SubagentRun>): SubagentRun => ({
  def: 'general',
  call_id: 'call-1',
  session: 's1',
  origin: 'subagent:general#call-1',
  state: 'done',
  started_at: '2026-11-07T00:00:00Z',
  ended_at: '2026-11-07T00:00:05Z',
  ...over,
})

beforeEach(() => {
  setActivePinia(createPinia())
  vi.restoreAllMocks()
})

function mountView(r: SubagentRun) {
  return mount(TranscriptView, { props: { run: r }, global: { plugins: [i18n] } })
}

describe('TranscriptView', () => {
  it('renders the report as a log region under the sub-transcript testid', () => {
    const w = mountView(run({ report: 'line one\nline two' }))
    const log = w.get('[data-testid="sub-transcript-call-1"]')
    expect(log.attributes('role')).toBe('log')
    expect(log.text()).toBe('line one\nline two')
    expect(log.classes()).toContain('mono')
    expect(log.classes()).toContain('capped')
  })

  it('copies the report through the clipboard API', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    const w = mountView(run({ report: 'copy me' }))
    await w.get('[data-testid="sub-transcript-copy"]').trigger('click')
    expect(writeText).toHaveBeenCalledWith('copy me')
  })

  it('running/waiting children show the live placeholder, never an empty log', () => {
    const w = mountView(run({ state: 'running', ended_at: null, report: undefined }))
    expect(w.get('[data-testid="sub-transcript-call-1"]').text()).toBe(
      i18n.global.t('activity.transcriptLive'),
    )
  })
})
