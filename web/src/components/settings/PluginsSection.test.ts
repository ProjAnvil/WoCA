import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  putPlugins: vi.fn(),
  installPlugin: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import PluginsSection from './PluginsSection.vue'
import { useSettingsStore } from '../../stores/settings'
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
    const settings = useSettingsStore()
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
