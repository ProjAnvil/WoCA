import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatPanel from './ChatPanel.vue'
import { useSessionStore } from '../stores/sessions'

const stubs = { ChatItemView: true }

function mountPanel() {
  return mount(ChatPanel, { global: { plugins: [i18n, createPinia()], stubs } })
}

function seedWithMessage() {
  const s = useSessionStore()
  s.seed(
    [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] },
  )
  return s
}

describe('composer FLIP dock transition', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('skips the FLIP entirely under prefers-reduced-motion', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }))
    const wrapper = mountPanel()
    seedWithMessage()
    await new Promise((r) => setTimeout(r, 0))
    const root = wrapper.get('.chat-input').element as HTMLElement
    expect(root.style.transform).toBe('')
  })

  it('compensates the layout jump with an invert transform before play', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }))
    let calls = 0
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function () {
      calls++
      return { ...({} as DOMRect), top: calls === 1 ? 300 : 600 } as DOMRect
    })
    const wrapper = mountPanel()
    calls = 0 // reset after mount: the first call is the First measurement in the watch callback
    seedWithMessage()
    await new Promise((r) => setTimeout(r, 0))
    // happy-dom's rAF is a real 16ms timer: at assertion time the two frames
    // have not drained, so the transform must sit on the Invert compensation value.
    const root = wrapper.get('.chat-input').element as HTMLElement
    expect(root.style.transform).toBe('translateY(-300px)')
    expect(root.style.transition).toContain('none')
  })
})
