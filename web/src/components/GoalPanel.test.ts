// Goal panel chrome: the header close button is an icon-only control and
// must carry an accessible name (ui-audit 2026-11-17).
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'
import i18n from '../i18n'
import GoalPanel from './GoalPanel.vue'
import { useSessionStore } from '../stores/sessions'

let w: ReturnType<typeof mount> | null = null

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('GoalPanel', () => {
  it('labels the close button and closes the panel on click', async () => {
    const store = useSessionStore()
    store.goalPanelOpen = true
    w = mount(GoalPanel, { global: { plugins: [i18n] }, attachTo: document.body })
    const close = w.get('.card-head .icon-btn')
    expect(close.attributes('aria-label')).toBe(i18n.global.t('common.close'))
    expect(close.attributes('type')).toBe('button')
    await close.trigger('click')
    expect(store.goalPanelOpen).toBe(false)
    w.unmount()
    w = null
    document.body.innerHTML = ''
  })
})
