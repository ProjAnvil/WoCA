import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { describe, expect, it } from 'vitest'
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
