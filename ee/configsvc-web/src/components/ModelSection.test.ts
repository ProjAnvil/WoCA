import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { flushPromises } from '@vue/test-utils'
import ModelSection from './ModelSection.vue'
import i18n from '../i18n'

// Preset-manager Model section: lists operator presets, no "active" flag.
function mountWith(presets: unknown[]) {
  const saved: Array<{ method?: string; path?: string; body?: unknown }> = []
  const api = async (path: string, init?: RequestInit) => {
    saved.push({ method: init?.method, path, body: init?.body })
    if (path === '/api/models' && (!init || init.method === undefined)) {
      return new Response(JSON.stringify(presets), { status: 200 })
    }
    return new Response(JSON.stringify(presets[0] ?? {}), { status: 200 })
  }
  const w = mount(ModelSection, {
    global: {
      plugins: [i18n],
      provide: {
        cfg: {
          s: null, d: () => '', v: () => '', setDraft: () => {},
          save: async () => {}, saving: false, savedFlash: '', error: '', api,
        },
      },
    },
  })
  return { w, saved }
}

describe('ModelSection (presets)', () => {
  it('renders the preset list without an active marker', async () => {
    const { w } = mountWith([
      { id: 'mp-1', name: 'prod', provider: 'glm', model: 'glm-5', max_tokens: 4096, temperature: 0, base_url: '', key_set: true, updated_at: 1 },
      { id: 'mp-2', name: 'cheap', provider: 'deepseek', model: 'deepseek-chat', max_tokens: 4096, temperature: 0, base_url: '', key_set: false, updated_at: 2 },
    ])
    await flushPromises()
    expect(w.text()).toContain('prod')
    expect(w.text()).toContain('glm · glm-5')
    expect(w.text()).toContain('cheap')
  })

  it('opens a blank editor on new-preset and saves via POST', async () => {
    const { w, saved } = mountWith([])
    await flushPromises()
    await w.findAll('button').find(b => b.text().includes('New'))!.trigger('click')
    const name = w.find('input')
    await name.setValue('my preset')
    const save = w.findAll('button').find(b => b.text() === 'Save')!
    await save.trigger('click')
    await flushPromises()
    const post = saved.find(c => c.method === 'POST' && c.path === '/api/models')
    expect(post).toBeTruthy()
    expect(JSON.parse(String(post!.body)).name).toBe('my preset')
  })
})
