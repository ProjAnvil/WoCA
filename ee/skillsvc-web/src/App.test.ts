import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import en from './i18n/locales/en'
import zhCN from './i18n/locales/zh-CN'
import App from './App.vue'

// App renders the asset list and locale toggle; switching locale changes copy.
describe('skill-svc console', () => {
  const i18n = createI18n({ legacy: false, locale: 'en', messages: { en, 'zh-CN': zhCN } })

  beforeEach(() => {
    vi.restoreAllMocks()
    i18n.global.locale.value = 'en'
    globalThis.fetch = vi.fn().mockResolvedValue({ json: () => [] }) as any
  })

  it('renders the title in English by default', async () => {
    const app = App
    const w = mount(app, { global: { plugins: [i18n] } })
    await flushPromises()
    expect(w.text()).toContain('Skill Assets')
  })

  it('toggles to Chinese', async () => {
    const app = App
    const w = mount(app, { global: { plugins: [i18n] } })
    await flushPromises()
    await w.find('.locale-toggle').trigger('click')
    expect(w.text()).toContain('技能资产')
  })

  it('shows the empty state when no assets', async () => {
    const app = App
    const w = mount(app, { global: { plugins: [i18n] } })
    await flushPromises()
    expect(w.text()).toContain('No assets yet')
  })
})
