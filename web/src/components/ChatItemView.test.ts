import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatItemView from './ChatItemView.vue'
import type { ChatItem } from '../types'

function mountItem(item: ChatItem) {
  return mount(ChatItemView, {
    props: { item },
    global: { plugins: [i18n, createPinia()] },
  })
}

const user = (fresh?: boolean): ChatItem =>
  ({ id: 'u1', kind: 'user', content: 'hi', ...(fresh ? { fresh } : {}) }) as ChatItem

describe('user message entrance marking', () => {
  it('marks fresh live-pushed user bubbles', () => {
    const w = mountItem(user(true))
    expect(w.get('.message').classes()).toContain('message--new')
  })
  it('does not mark history-loaded user bubbles', () => {
    const w = mountItem(user())
    expect(w.get('.message').classes()).not.toContain('message--new')
  })
})

const assistant = (content = '**hi** _world_'): ChatItem =>
  ({ id: 'a1', kind: 'assistant', content, streaming: false }) as ChatItem

function mountAssistant(content?: string) {
  return mountItem(assistant(content))
}

function stubClipboard() {
  const write = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
  return write
}

describe('bubble copy split button', () => {
  it('main button copies the markdown source', async () => {
    const write = stubClipboard()
    const w = mountAssistant()
    await w.get('.bubble-action.copy-main').trigger('click')
    expect(write).toHaveBeenCalledWith('**hi** _world_')
  })

  it('dropdown plain-text item copies stripped text', async () => {
    const write = stubClipboard()
    const w = mountAssistant()
    try {
      await w.get('.bubble-action.copy-side').trigger('click') // span 上 toggle 打开
      // 菜单 Teleport 到 body——从 document 查（wrapper 查不到）
      const items = Array.from(document.body.querySelectorAll('[role="menuitem"]'))
      expect(items).toHaveLength(2)
      items[1].dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await w.vm.$nextTick()
      expect(write).toHaveBeenCalledWith('hi world')
    } finally {
      w.unmount()
    }
  })

  it('pins the action bar while the menu is open', async () => {
    const w = mountAssistant()
    expect(w.get('.bubble-actions').classes()).not.toContain('is-menu-open')
    await w.get('.bubble-action.copy-side').trigger('click')
    expect(w.get('.bubble-actions').classes()).toContain('is-menu-open')
  })

  it('hides the action bar while still streaming', () => {
    const w = mountItem({ id: 'a1', kind: 'assistant', content: 'partial', streaming: true } as ChatItem)
    expect(w.find('.bubble-actions').exists()).toBe(false)
  })
})

describe('reasoning card meta', () => {
  const reasoning = (over: Record<string, unknown>): ChatItem =>
    ({ id: 'r1', kind: 'reasoning', content: 'think', streaming: false, ...over }) as ChatItem

  it('ticks live seconds while the thinking segment streams', () => {
    const w = mountItem(reasoning({ streaming: true, startedAt: Date.now() }))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.thinkingSeconds', { count: 0 }))
    w.unmount()
  })

  it('shows the total once known', () => {
    const w = mountItem(reasoning({ durationMs: 3000 }))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.thoughtFor', { count: 3 }))
  })

  it('falls back to the char count for legacy blocks without durations', () => {
    const w = mountItem(reasoning({}))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.charCount', { count: 5 }))
  })
})
