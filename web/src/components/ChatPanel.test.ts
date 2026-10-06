import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatPanel from './ChatPanel.vue'
import { useSessionStore } from '../stores/sessions'
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
