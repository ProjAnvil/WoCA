// Task 11: the multi-card approval stack — parallel cards (never a queue),
// the >3 fold into an "N more" reveal, the session button's rule-key preview,
// the three testid-addressable buttons, and the grants footnote chip.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  answerRun: vi.fn(),
  listGrants: vi.fn(),
  revokeGrant: vi.fn(),
}))
vi.mock('../../api/client', () => ({ ...api }))

import i18n from '../../i18n'
import HitlStack from './HitlStack.vue'
import { useSessionStore } from '../../stores/sessions'
import type { InterruptCardData } from '../../types'

function permCard(id: string, ns: string, over: Record<string, unknown> = {}): InterruptCardData {
  return {
    id,
    ns,
    kind: 'permission',
    timedOut: false,
    payload: {
      tool_name: 'Bash',
      tool_args: { command: `git ${id}` },
      reason: 'run',
      is_read_only: false,
      asks: [
        {
          call_id: id,
          tool_name: 'Bash',
          tool_args: { command: `git ${id}` },
          reason: 'run',
          origin: 'main',
          session_rule_key: `bash:git ${id}*`,
          ...over,
        },
      ],
    },
  }
}

function mountStack(cards: InterruptCardData[]) {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.seed(
    [{ id: 'session-1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    { 'session-1': [] },
  )
  store.interrupt['session-1'] = cards
  store.runId['session-1'] = 'run-1'
  const w = mount(HitlStack, { global: { plugins: [i18n, pinia] } })
  return { w, store }
}

const flush = async (w: { vm: { $nextTick: () => Promise<void> } }) => {
  await Promise.resolve()
  await Promise.resolve()
  await w.vm.$nextTick()
}

beforeEach(() => {
  vi.clearAllMocks()
  api.answerRun.mockReset().mockResolvedValue(undefined)
  api.listGrants.mockReset().mockResolvedValue({ grants: [] })
})

describe('approval stack rendering', () => {
  it('renders every pending card in parallel with the testid contract', async () => {
    const { w } = mountStack([
      permCard('call-aaaa', 'approval:se1'),
      { id: 'call-bbbb', ns: 'subagent:researcher-ad33:se2', kind: 'permission', payload: { subagent: 'researcher', tool_name: 'WebFetch', tool_args: { url: 'https://example.com' }, reason: 'docs', is_read_only: true } },
    ])
    await flush(w)
    const stack = w.get('[data-testid="hitl-stack"]')
    expect(stack.attributes('role')).toBe('dialog')
    expect(w.find('[data-testid="hitl-card-call-aaaa"]').exists()).toBe(true)
    expect(w.find('[data-testid="hitl-card-call-bbbb"]').exists()).toBe(true)
    // Cards are listitems-equivalent groups labelled by their tool name.
    expect(w.get('[data-testid="hitl-card-call-aaaa"]').attributes('role')).toBe('group')
    expect(w.get('[data-testid="hitl-card-call-bbbb"]').text()).toContain('WebFetch')
  })

  it('folds cards beyond three into an "N more" reveal row', async () => {
    const cards = ['aaaa', 'bbbb', 'cccc', 'dddd', 'eeee'].map((n, i) => permCard(`call-${n}`, `approval:se${i + 1}`))
    const { w } = mountStack(cards)
    await flush(w)
    const more = w.get('[data-testid="hitl-queue-more"]')
    expect(more.text()).toContain('2')
    // The folded cards live in the collapsed reveal container.
    const fold = w.get('[data-testid="hitl-card-call-dddd"]').element.closest('.hitl-fold')
    expect(fold?.classList.contains('open')).toBe(false)
    await more.trigger('click')
    expect(fold?.classList.contains('open')).toBe(true)
  })

  it('shows the origin badge: main vs the subagent def name', async () => {
    const { w } = mountStack([
      permCard('call-aaaa', 'approval:se1'),
      { id: 'call-bbbb', ns: 'subagent:researcher-ad33:se2', kind: 'permission', payload: { subagent: 'researcher', tool_name: 'WebFetch', tool_args: {}, reason: '', is_read_only: true } },
    ])
    await flush(w)
    const main = w.get('[data-testid="hitl-card-call-aaaa"]').text()
    expect(main).toContain(i18n.global.t('chat.sourceMain'))
    expect(w.get('[data-testid="hitl-card-call-bbbb"]').text()).toContain('researcher')
  })
})

describe('card answering', () => {
  it('allow-once posts the per-card decision without a grant', async () => {
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    await w.get('[data-testid="hitl-allow-call-aaaa"]').trigger('click')
    await flush(w)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', {
      answers: [{ key: 'approval:se1', value: { decisions: { 'call-aaaa': { outcome: 'allow' } } } }],
    })
  })

  it('allow-for-session posts the session grant and the card previews the rule key', async () => {
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    expect(w.get('[data-testid="hitl-rule-key-call-aaaa"]').text()).toContain('bash:git call-aaaa*')
    await w.get('[data-testid="hitl-grant-session-call-aaaa"]').trigger('click')
    await flush(w)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', {
      answers: [{ key: 'approval:se1', value: { decisions: { 'call-aaaa': { outcome: 'allow', grant: 'session' } } } }],
    })
  })

  it('deny posts a deny decision (the per-card cancel ruling)', async () => {
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    await w.get('[data-testid="hitl-deny-call-aaaa"]').trigger('click')
    await flush(w)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', {
      answers: [{ key: 'approval:se1', value: { decisions: { 'call-aaaa': { outcome: 'deny' } } } }],
    })
  })

  it('legacy cards without asks answer with the flat approve flag', async () => {
    const { w } = mountStack([
      { id: 'call-bbbb', ns: 'subagent:researcher-ad33:se2', kind: 'permission', payload: { subagent: 'researcher', tool_name: 'WebFetch', tool_args: {}, reason: '', is_read_only: true } },
    ])
    await flush(w)
    // No rule key on legacy cards: the session button stays hidden.
    expect(w.find('[data-testid="hitl-grant-session-call-bbbb"]').exists()).toBe(false)
    await w.get('[data-testid="hitl-allow-call-bbbb"]').trigger('click')
    await flush(w)
    expect(api.answerRun).toHaveBeenCalledWith('run-1', {
      answers: [{ key: 'subagent:researcher-ad33:se2', value: { approve: true } }],
    })
  })

  it('a failed answer surfaces an inline alert and retries', async () => {
    api.answerRun.mockRejectedValueOnce(new Error('boom'))
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    await w.get('[data-testid="hitl-allow-call-aaaa"]').trigger('click')
    await flush(w)
    expect(w.get('[data-testid="hitl-card-call-aaaa"]').text()).toContain('boom')
    // Retry: the buttons work again.
    await w.get('[data-testid="hitl-allow-call-aaaa"]').trigger('click')
    await flush(w)
    expect(api.answerRun).toHaveBeenCalledTimes(2)
  })
})

describe('grants footnote', () => {
  it('shows the grants chip with the session grant count and opens the panel', async () => {
    api.listGrants.mockResolvedValue({ grants: [{ key: 'bash:git push*' }, { key: 'domain:example.com' }] })
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    const chip = w.get('[data-testid="grants-chip"]')
    expect(chip.text()).toContain('2')
    await chip.trigger('click')
    expect(w.findComponent({ name: 'GrantsPanel' }).exists()).toBe(true)
  })
})

describe('approval readability fixes (375px audit)', () => {
  it('renders the head counter: last visible index over total, fold included', async () => {
    const cards = ['aaaa', 'bbbb', 'cccc', 'dddd', 'eeee'].map((n, i) => permCard(`call-${n}`, `approval:se${i + 1}`))
    const { w } = mountStack(cards)
    await flush(w)
    const count = w.get('[data-testid="hitl-stack-count"]')
    // Collapsed: three cards in view of five pending.
    expect(count.text()).toBe(i18n.global.t('chat.cardPosition', { n: 3, m: 5 }))
    await w.get('[data-testid="hitl-queue-more"]').trigger('click')
    // The fold reveal counts: every card is now in view.
    expect(count.text()).toBe(i18n.global.t('chat.cardPosition', { n: 5, m: 5 }))
  })

  it('wraps long args in a capped pre with an expand toggle; short args get none', async () => {
    const longCommand = `sleep 6 && run_in_background ${'x'.repeat(400)}`
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1', { tool_args: { command: longCommand } })])
    await flush(w)
    const card = w.get('[data-testid="hitl-card-call-aaaa"]')
    const pre = card.get('pre.hitl-tool-args')
    // Collapsed by default; the full command text is in the DOM.
    expect(pre.classes()).toContain('is-collapsed')
    expect(pre.text()).toContain('run_in_background')
    const toggle = w.get('[data-testid="hitl-args-toggle-call-aaaa"]')
    expect(toggle.attributes('aria-expanded')).toBe('false')
    await toggle.trigger('click')
    expect(w.get('[data-testid="hitl-args-toggle-call-aaaa"]').attributes('aria-expanded')).toBe('true')
    expect(card.get('pre.hitl-tool-args').classes()).not.toContain('is-collapsed')
    // Short args never overflow the 8-line cap: no toggle noise.
    const { w: short } = mountStack([permCard('call-bbbb', 'approval:se2')])
    await flush(short)
    expect(short.find('[data-testid="hitl-args-toggle-call-bbbb"]').exists()).toBe(false)
  })

  it('renders the rule key as its own line outside the session button; testids survive', async () => {
    const { w } = mountStack([permCard('call-aaaa', 'approval:se1')])
    await flush(w)
    const session = w.get('[data-testid="hitl-grant-session-call-aaaa"]')
    const key = w.get('[data-testid="hitl-rule-key-call-aaaa"]')
    expect(key.text()).toContain('bash:git call-aaaa*')
    // The button keeps just its label; the key lives outside any button.
    expect(session.text().trim()).toBe(i18n.global.t('chat.approveSession'))
    expect(key.element.closest('button')).toBe(null)
    // The three action testids (e2e contract) are unchanged.
    for (const tid of ['hitl-deny-call-aaaa', 'hitl-grant-session-call-aaaa', 'hitl-allow-call-aaaa']) {
      expect(w.find(`[data-testid="${tid}"]`).exists()).toBe(true)
    }
  })
})
