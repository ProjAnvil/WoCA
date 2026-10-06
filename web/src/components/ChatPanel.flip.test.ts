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
    calls = 0 // mount 后归零：首个调用即 watch 回调里的 First 量测
    seedWithMessage()
    await new Promise((r) => setTimeout(r, 0))
    // happy-dom 的 rAF 是真实 16ms 定时器：断言时双帧尚未 drain，transform
    // 必须停在 Invert 相的补偿值上——这正是被测行为。
    const root = wrapper.get('.chat-input').element as HTMLElement
    expect(root.style.transform).toBe('translateY(-300px)')
    expect(root.style.transition).toContain('none')
  })
})
