import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatPanel from './ChatPanel.vue'
import { useSessionStore } from '../stores/sessions'
import { useActivityStore } from '../stores/activity'
import { greetingSlot } from '../lib/greeting'

// The panel renders the full chat surface; the item view is irrelevant to
// keyboard-submission behavior.
const stubs = { ChatItemView: true }

function mountPanel() {
  return mount(ChatPanel, {
    global: { plugins: [i18n, createPinia()], stubs },
  })
}

async function draftEnter(wrapper: ReturnType<typeof mountPanel>, isComposing: boolean) {
  const textarea = wrapper.get('textarea')
  await textarea.setValue('hello')
  await textarea.trigger('keydown', { key: 'Enter', shiftKey: false, isComposing })
}

describe('ChatPanel message submission', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('submits on Enter when no IME composition is active', async () => {
    const wrapper = mountPanel()
    const sendSpy = vi.spyOn(useSessionStore(), 'send').mockImplementation(async () => {})

    await draftEnter(wrapper, false)

    expect(sendSpy).toHaveBeenCalledTimes(1)
    expect(sendSpy).toHaveBeenCalledWith('hello', [], [])
  })

  it('keeps the draft when Enter confirms an IME composition', async () => {
    const wrapper = mountPanel()
    const sendSpy = vi.spyOn(useSessionStore(), 'send').mockImplementation(async () => {})

    await draftEnter(wrapper, true)

    expect(sendSpy).not.toHaveBeenCalled()
    expect(wrapper.get('textarea').element.value).toBe('hello')
  })
})

describe('ChatPanel greeting state', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the centered greeting state on an empty session', () => {
    const wrapper = mountPanel()
    expect(wrapper.get('.chat').classes()).toContain('chat--greeting')
    expect(wrapper.get('.chat-greeting-text').text()).toBe(
      i18n.global.t(`chat.greeting.${greetingSlot()}`),
    )
  })

  it('fills the composer from a greeting chip prompt', async () => {
    const wrapper = mount(ChatPanel, {
      global: { plugins: [i18n, createPinia()] },
      stubs,
      attachTo: document.body,
    })
    try {
      await wrapper.get('.greeting-chip').trigger('click')
      expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).not.toBe('')
      expect(document.activeElement).toBe(wrapper.get('textarea').element)
    } finally {
      wrapper.unmount()
    }
  })

  it('leaves the greeting state once a message exists', async () => {
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
    )
    await wrapper.vm.$nextTick()
    expect(wrapper.get('.chat').classes()).not.toContain('chat--greeting')
    expect(wrapper.find('.chat-greeting').exists()).toBe(false)
  })
})

describe('ChatPanel first-output shimmer', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the shimmer while a run has produced nothing yet', async () => {
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi', fresh: true }] },
    )
    s.status = { s1: 'running' }
    await wrapper.vm.$nextTick()
    expect(wrapper.find('.msg-shimmer-row').exists()).toBe(true)
  })

  it('hides the shimmer once any output block exists', async () => {
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      {
        s1: [
          { id: 'u1', kind: 'user', content: 'hi' },
          { id: 'a1', kind: 'assistant', content: 'h', streaming: true },
        ],
      },
    )
    s.status = { s1: 'running' }
    await wrapper.vm.$nextTick()
    expect(wrapper.find('.msg-shimmer-row').exists()).toBe(false)
  })
})

describe('ChatPanel activity header entry', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the activity trigger immediately left of the Memory button', () => {
    const wrapper = mountPanel()
    const header = wrapper.get('.chat-header').element
    const kids = Array.from(header.children)
    const act = kids.findIndex((el) => el.classList.contains('act-trigger'))
    const mem = kids.findIndex((el) => el.classList.contains('mem-toggle'))
    expect(act).toBeGreaterThanOrEqual(0)
    expect(mem).toBeGreaterThanOrEqual(0)
    expect(act).toBe(mem - 1)
  })

  it('marks auto-wake runs in the header running pill', async () => {
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
    )
    s.status = { s1: 'running' }
    s.runAuto['s1'] = true
    await wrapper.vm.$nextTick()
    expect(wrapper.get('.pill-running .pill-auto').text()).toBe(i18n.global.t('chat.autoRun'))
    expect(wrapper.find('.pill-running .lucide-zap').exists()).toBe(true)
  })

  it('omits the auto marker for user-initiated runs', async () => {
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.seed(
      [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
      { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
    )
    s.status = { s1: 'running' }
    s.runAuto['s1'] = false
    await wrapper.vm.$nextTick()
    expect(wrapper.find('.pill-auto').exists()).toBe(false)
  })

  it('focuses the composer after the capped strip "send a message" action', async () => {
    const wrapper = mount(ChatPanel, {
      global: { plugins: [i18n, createPinia()] },
      stubs,
      attachTo: document.body,
    })
    try {
      const s = useSessionStore()
      s.seed(
        [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
        { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
      )
      // A capped wake state without any backend round-trip.
      useActivityStore().bySession['s1'] = {
        snapshot: {
          session_id: 's1',
          server_time: new Date().toISOString(),
          todos: { items: [], updated_at: null },
          shells: [],
          tasks: [],
          crons: [],
          wake: { enabled: true, capped: true, stalled: false, pending_notifications: 1 },
        },
        loading: false,
        error: '',
        unavailable: false,
        stale: false,
      }
      s.activityPanelOpen = true
      await wrapper.vm.$nextTick()
      await wrapper.vm.$nextTick()
      const pop = document.querySelector('.act-pop') as HTMLElement
      expect(pop).toBeTruthy()
      ;(pop.querySelector('[data-tab="scheduled"]') as HTMLElement).click()
      await wrapper.vm.$nextTick()
      ;(pop.querySelector('.act-wake-action') as HTMLElement).click()
      await wrapper.vm.$nextTick()
      // panel closed, composer focused
      expect(document.querySelector('.act-pop')).toBeNull()
      expect(document.activeElement).toBe(wrapper.get('textarea').element)
    } finally {
      wrapper.unmount()
      document.body.innerHTML = ''
    }
  })
})
