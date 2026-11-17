// Memory panel chrome (ui-audit 2026-11-17): the per-item forget button is
// a small glyph and must be announced (memory.forget), and the card close
// button needs its accessible name.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'
import i18n from '../i18n'
import MemoryPanel from './MemoryPanel.vue'
import { useMemoryStore } from '../stores/memory'
import type { MemoryItem } from '../types'

const item = (id: string, layer: string): MemoryItem => ({
  id,
  layer,
  type: 'fact',
  summary: `summary-${id}`,
  content: '',
  source: 'test',
  tags: null,
  updated_at: 1_760_000_000,
})

describe('MemoryPanel', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  function mountWithItems(items: MemoryItem[]) {
    const w = mount(MemoryPanel, { global: { plugins: [i18n] } })
    const memory = useMemoryStore()
    memory.items = items
    return { w, memory }
  }

  it('labels every forget button, including the insights group', async () => {
    const { w } = mountWithItems([
      item('a', 'L2_semantic'),
      item('b', 'L5_mental'),
      item('c', 'L6_predictive'),
    ])
    await w.vm.$nextTick()
    const dels = w.findAll('.mem-del')
    expect(dels).toHaveLength(3)
    for (const del of dels) {
      expect(del.element.tagName).toBe('BUTTON')
      expect(del.attributes('aria-label')).toBe(i18n.global.t('memory.forget'))
    }
  })

  it('labels the card close button', async () => {
    const { w, memory } = mountWithItems([])
    await w.vm.$nextTick()
    const close = w.get('.card-head .icon-btn')
    expect(close.attributes('aria-label')).toBe(i18n.global.t('common.close'))
    await close.trigger('click')
    expect(memory.panelOpen).toBe(false)
  })
})
