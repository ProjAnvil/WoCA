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
})
