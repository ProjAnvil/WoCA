// Task 12: the Subagents tab — the session's foreground subagent children
// as an in-panel list (not a tree): state machine rendering (color+text
// dual channel), elapsed/tokens, expand-to-transcript (Enter/Escape),
// the empty state, and the waiting row's go-to-approval focus affordance.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import SubagentTab from './SubagentTab.vue'
import { useActivityStore } from '../../stores/activity'
import { useSessionStore } from '../../stores/sessions'
import type { ActivitySnapshot, InterruptCardData, SubagentRun } from '../../types'

const api = vi.hoisted(() => ({
  getActivity: vi.fn(),
  getShellOutput: vi.fn(),
  getTaskOutput: vi.fn(),
  killShell: vi.fn(),
  stopTask: vi.fn(),
  deleteCron: vi.fn(),
  answerRun: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  ...api,
}))

const flush = async () => {
  for (let i = 0; i < 10; i++) await Promise.resolve()
}

const run = (
  call_id: string,
  state: SubagentRun['state'],
  over: Partial<SubagentRun> = {},
): SubagentRun => ({
  def: 'general',
  call_id,
  session: 's1',
  origin: `subagent:general#${call_id}`,
  state,
  started_at: '2026-11-07T00:00:00Z',
  ended_at: state === 'running' || state === 'waiting' ? null : '2026-11-07T00:00:05Z',
  ...over,
})

const snap = (subs: SubagentRun[]): ActivitySnapshot => ({
  session_id: 's1',
  server_time: '2026-11-07T00:00:00Z',
  todos: { items: [], updated_at: null },
  shells: [],
  tasks: [],
  crons: [],
  subagents: subs,
  wake: { enabled: false, capped: false, stalled: false, pending_notifications: 0 },
})

const states: Array<SubagentRun['state']> = ['running', 'waiting', 'done', 'failed', 'stopped']

async function mountTab(subs: SubagentRun[], attach = false) {
  const pinia = createPinia()
  setActivePinia(pinia)
  const activity = useActivityStore()
  api.getActivity.mockResolvedValueOnce(snap(subs))
  await activity.fetchNow('s1')
  const w = mount(SubagentTab, {
    props: { sid: 's1' },
    global: { plugins: [i18n, pinia] },
    ...(attach ? { attachTo: document.body } : {}),
  })
  await flush()
  return w
}

beforeEach(() => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('SubagentTab rows', () => {
  it('renders one row per child with the testid contract and state texts', async () => {
    const w = await mountTab(states.map((st, i) => run(`call-${i + 1}`, st)))
    for (let i = 1; i <= states.length; i++) {
      expect(w.find(`[data-testid="sub-row-call-${i}"]`).exists()).toBe(true)
      expect(w.find(`[data-testid="sub-status-call-${i}"]`).exists()).toBe(true)
    }
    for (const [i, st] of states.entries()) {
      expect(w.get(`[data-testid="sub-status-call-${i + 1}"]`).text()).toBe(
        i18n.global.t(`activity.subStatus.${st}`),
      )
    }
    // Dual channel: the running dot breathes (brand), no dot text lost.
    expect(w.find('[data-testid="sub-status-call-1"] .act-breathe').exists()).toBe(true)
    expect(w.find('[data-testid="sub-status-call-2"] .act-breathe').exists()).toBe(false)
  })

  it('renders tokens in tabular numerals when the child reported usage', async () => {
    const w = await mountTab([run('call-1', 'done', { tokens: 4321 })])
    const tokens = w.get('[data-testid="sub-row-call-1"] .sub-row-tokens')
    expect(tokens.text()).toBe('4,321')
    expect(tokens.classes()).toContain('tabular')
  })

  it('shows the empty state when the session has no children', async () => {
    const w = await mountTab([])
    expect(w.get('[data-testid="sub-empty"]').text()).toBe(i18n.global.t('activity.subagentsEmpty'))
    expect(w.find('[data-testid="sub-row-call-1"]').exists()).toBe(false)
  })
})

describe('SubagentTab transcript expansion', () => {
  it('clicking a row reveals the readonly transcript; clicking again collapses', async () => {
    const w = await mountTab([run('call-1', 'done', { report: 'final words' })])
    const row = w.get('[data-testid="sub-row-call-1"]')
    expect(row.attributes('aria-expanded')).toBe('false')
    await row.trigger('click')
    const log = w.get('[data-testid="sub-transcript-call-1"]')
    expect(log.attributes('role')).toBe('log')
    expect(log.text()).toContain('final words')
    expect(w.get('[data-testid="sub-row-call-1"]').attributes('aria-expanded')).toBe('true')
    await row.trigger('click')
    expect(w.find('[data-testid="sub-transcript-call-1"]').exists()).toBe(false)
  })

  it('Enter expands the focused row and Escape collapses it (keyboard contract)', async () => {
    // attached: the roving model reads document.activeElement, which jsdom
    // only maintains for elements in the document.
    const w = await mountTab([run('call-1', 'running'), run('call-2', 'done')], true)
    const rows = w.findAll('[data-row-key]')
    ;(rows[0].element as HTMLElement).focus()
    await rows[0].trigger('keydown', { key: 'Enter' })
    expect(w.find('[data-testid="sub-transcript-call-1"]').exists()).toBe(true)
    await rows[0].trigger('keydown', { key: 'Escape' })
    expect(w.find('[data-testid="sub-transcript-call-1"]').exists()).toBe(false)
    // ArrowDown moves focus to the next row (re-queried: the expansion
    // re-render may replace the row elements).
    const rowsNow = w.findAll('[data-row-key]')
    await rowsNow[0].trigger('keydown', { key: 'ArrowDown' })
    expect(document.activeElement).toBe(rowsNow[1].element)
    await rowsNow[1].trigger('keydown', { key: 'ArrowUp' })
    expect(document.activeElement).toBe(w.findAll('[data-row-key]')[0].element)
  })

  it('a running child shows the live placeholder instead of a report', async () => {
    const w = await mountTab([run('call-1', 'running')])
    await w.get('[data-testid="sub-row-call-1"]').trigger('click')
    expect(w.get('[data-testid="sub-transcript-call-1"]').text()).toBe(
      i18n.global.t('activity.transcriptLive'),
    )
  })
})

describe('SubagentTab go-to-approval affordance', () => {
  it('a waiting row embeds a button that closes the panel and focuses that child\'s card', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const activity = useActivityStore()
    api.getActivity.mockResolvedValueOnce(snap([run('call-2', 'waiting', { def: 'researcher' })]))
    await activity.fetchNow('s1')

    // The waiting child's pause card: asks carry origin = the def name.
    const card: InterruptCardData = {
      id: 'child-ask-9',
      ns: 'subagent:researcher-ad33:tools',
      kind: 'approvals',
      payload: {
        asks: [
          {
            call_id: 'child-ask-9',
            tool_name: 'WebFetch',
            origin: 'researcher',
          },
        ],
      },
    }
    const sessions = useSessionStore()
    sessions.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [] },
    )
    sessions.interrupt['s1'] = [card]
    sessions.activityPanelOpen = true

    // The HitlStack itself is not mounted here; its card anchor is.
    const anchor = document.createElement('div')
    anchor.setAttribute('data-testid', 'hitl-card-child-ask-9')
    anchor.tabIndex = -1
    document.body.appendChild(anchor)

    const w = mount(SubagentTab, {
      props: { sid: 's1' },
      global: { plugins: [i18n, pinia] },
      attachTo: document.body,
    })
    await flush()

    const btn = w.get('[data-testid="sub-goto-approval-call-2"]')
    expect(btn.text()).toBe(i18n.global.t('activity.subGotoApproval'))
    await btn.trigger('click')
    await flush()
    expect(sessions.activityPanelOpen).toBe(false)
    expect(document.activeElement).toBe(anchor)
    // Other-state rows carry no affordance.
    const w2 = await mountTab([run('call-1', 'running')])
    expect(w2.find('[data-testid="sub-goto-approval-call-1"]').exists()).toBe(false)
  })
})
