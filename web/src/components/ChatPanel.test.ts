import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatPanel from './ChatPanel.vue'
import { useSessionStore } from '../stores/sessions'

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
    expect(sendSpy).toHaveBeenCalledWith('hello', [])
  })

  it('keeps the draft when Enter confirms an IME composition', async () => {
    const wrapper = mountPanel()
    const sendSpy = vi.spyOn(useSessionStore(), 'send').mockImplementation(async () => {})

    await draftEnter(wrapper, true)

    expect(sendSpy).not.toHaveBeenCalled()
    expect(wrapper.get('textarea').element.value).toBe('hello')
  })
})
