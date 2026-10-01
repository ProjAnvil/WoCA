import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  putPlugins: vi.fn(),
  installPlugin: vi.fn(),
  getSettings: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import PluginsSection from './PluginsSection.vue'
import { useServerSettingsStore } from '../../stores/serverSettings'
import type { AppSettings, PluginStatus } from '../../types'

function plugin(over: Partial<PluginStatus> = {}): PluginStatus {
  return {
    name: 'browser',
    kind: 'golang',
    version: '0.1.0',
    description: 'drives a browser',
    tools: ['BrowserNavigate'],
    runtime: 'pinchtab',
    enabled: false,
    state: 'ready',
    ...over,
  } as PluginStatus
}

function mountSection(pinia: ReturnType<typeof createPinia>, bridge?: boolean) {
  return mount(PluginsSection, {
    props: { bridge },
    global: { plugins: [i18n, pinia] },
  })
}

describe('PluginsSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
  })

  it('renders the plugin cards and toggles through the store', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    api.getPlugins.mockResolvedValue({ plugins: [plugin()] })
    api.putPlugins.mockResolvedValue({})
    const w = mountSection(pinia)
    await flushPromises()

    expect(w.text()).toContain('browser')
    expect(w.text()).toContain('drives a browser')

    await w.get('.plugin-card .switch').trigger('click')
    await flushPromises()
    expect(api.putPlugins).toHaveBeenCalledWith(['browser'])
    expect(w.text()).toContain('Enabled browser')
  })

  it('marks org-managed plugins and freezes the toggles when the set is locked', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useServerSettingsStore()
    settings.server = { managed_fields: ['builtin-plugins'], msg_plane_available: false } as AppSettings
    api.getPlugins.mockResolvedValue({ plugins: [plugin({ enabled: true })] })

    const w = mountSection(pinia, true)
    await flushPromises()

    expect(w.find('.plugin-badge-org').text()).toBe('org-managed')
    const toggle = w.get('.plugin-card .switch')
    expect((toggle.element as HTMLButtonElement).disabled).toBe(true)
    expect(api.putPlugins).not.toHaveBeenCalled()
  })

  it('offers install for a runtime that is not ready', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    api.getPlugins.mockResolvedValue({ plugins: [plugin({ state: 'not_installed' })] })
    api.installPlugin.mockResolvedValue({})
    const w = mountSection(pinia)
    await flushPromises()

    const btn = w.get('.plugin-actions .btn')
    expect(btn.text()).toContain('Download & install')
    await btn.trigger('click')
    await flushPromises()
    expect(api.installPlugin).toHaveBeenCalledWith('browser', false)
    expect(w.text()).toContain('Installing')
  })
})

describe('PluginsSection langfuse card', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
  })

  async function withServer(over: Partial<AppSettings> = {}) {
    const pinia = createPinia()
    setActivePinia(pinia)
    api.getPlugins.mockResolvedValue({ plugins: [] })
    api.getSettings.mockResolvedValue({
      provider: 'anthropic',
      langfuse_enabled: true,
      langfuse_base_url: 'https://lf.example',
      langfuse_public_key: 'pk-lf',
      langfuse_secret_key_set: true,
      langfuse_console_managed: false,
      ...over,
    } as AppSettings)
    const settings = useServerSettingsStore()
    await settings.loadServer()
    const w = mount(PluginsSection, { props: {}, global: { plugins: [i18n, pinia] } })
    await flushPromises()
    return { w, settings }
  }

  it('shows the card on the personal form and reflects saved state', async () => {
    const { w } = await withServer()
    const card = w.find('[data-testid="langfuse-card"]')
    expect(card.exists()).toBe(true)
    const inputs = card.findAll('input')
    expect((inputs[0].element as HTMLInputElement).value).toBe('https://lf.example')
    expect((inputs[1].element as HTMLInputElement).value).toBe('pk-lf')
    expect((inputs[2].element as HTMLInputElement).placeholder).toContain('Saved')
  })

  it('hides the card when the console owns observability', async () => {
    const { w } = await withServer({ langfuse_console_managed: true })
    expect(w.find('[data-testid="langfuse-card"]').exists()).toBe(false)
  })
})
