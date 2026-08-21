import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import StorageSection from './StorageSection.vue'
import i18n from '../i18n'
import type { Settings } from '../types'

function base(over: Partial<Settings> = {}): Settings {
  return {
    provider: 'anthropic', model: 'm', max_tokens: 4096, temperature: '',
    anthropic_base_url: '', openai_base_url: '', glm_base_url: '', deepseek_base_url: '',
    checkpoint_backend: 'sqlite', memory_backend: 'sqlite', checkpoint_dir: '',
    database: 'pg', debug_log: false, memory_engine: 'remote', ladym_embedding: 'inherit',
    ladym_llm: 'inherit', ladym_topk: 6, ladym_autoretain: true, ladym_system2: false,
    ladym_system2_interval: 300, ladym_endpoint: '', ladym_api_username: '',
    postgres_url_set: false, ladym_api_password_set: false,
    checkpoint_pg_host: '', checkpoint_pg_port: '', checkpoint_pg_db: '', checkpoint_pg_user: '',
    checkpoint_pg_password_set: false,
    memory_pg_host: '', memory_pg_port: '', memory_pg_db: '', memory_pg_user: '',
    memory_pg_password_set: false,
    msg_plane_mode: 'pg', msg_plane_nats_url: '', msg_plane_subject: 'runs.dispatch',
    msg_plane_available: true,
    ...over,
  }
}

function mountWith(s: Settings, pre: Record<string, string> = {}) {
  // reactive so draft writes re-render, exactly like the app's ref-backed drafts
  const drafts = reactive<Record<string, string>>({ ...pre })
  const ctx = {
    s,
    d: (f: string) => drafts[f] ?? '',
    v: (f: string) => drafts[f] !== undefined ? drafts[f] : String((s as unknown as Record<string, unknown>)[f] ?? ''),
    setDraft: (f: string, val: string) => { drafts[f] = val },
    save: async () => {}, saving: false, savedFlash: '', error: '',
    api: (async () => new Response()),
  }
  return mount(StorageSection, { global: { plugins: [i18n], provide: { cfg: reactive(ctx) } } })
}

describe('StorageSection', () => {
  it('hides connection fields until postgres is selected (draft reacts immediately)', async () => {
    const w = mountWith(base())
    expect(w.text()).not.toContain('Host')
    // switching the checkpoint backend to postgres reveals its own fields
    await w.findAll('select')[0].setValue('postgres')
    expect(w.text()).toContain('Host')
    expect(w.text()).toContain('Password')
    // the memory block stays collapsed
    const memSelect = w.findAll('select')[1].element as HTMLSelectElement
    expect(memSelect.value).toBe('sqlite')
  })

  it('shows the checkpoint dir only for the file backend', async () => {
    const w = mountWith(base({ checkpoint_backend: 'file' }))
    expect(w.text()).toContain('Checkpoint directory')
    await w.findAll('select')[0].setValue('postgres')
    expect(w.text()).not.toContain('Checkpoint directory')
  })

  it('renders stored per-subsystem values', () => {
    const w = mountWith(base({ checkpoint_backend: 'postgres', checkpoint_pg_host: 'ck-primary', checkpoint_pg_user: 'cku' }))
    const host = w.findAll('input')[0].element as HTMLInputElement
    expect(host.value).toBe('ck-primary')
  })
})
