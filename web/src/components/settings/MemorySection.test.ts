import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

import MemorySection from './MemorySection.vue'
import { useSettingsStore } from '../../stores/settings'
import type { AppSettings } from '../../types'

function baseServer(managed: string[] = []): AppSettings {
  return { managed_fields: managed, msg_plane_available: false } as AppSettings
}

function mountSection(pinia: ReturnType<typeof createPinia>) {
  return mount(MemorySection, { global: { plugins: [i18n, pinia] } })
}

describe('MemorySection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })

  it('offers the remote engine alongside the local and off options', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useSettingsStore()
    settings.server = baseServer()
    settings.memoryEngine = 'remote'
    const w = mountSection(pinia)
    const engine = w.findAll('select.field')[0]
    const values = engine.findAll('option').map((o) => (o.element as HTMLOptionElement).value)
    expect(values).toEqual(['ladym', 'remote', 'none'])
    expect((engine.element as HTMLSelectElement).value).toBe('remote')
    // The ladym-only tuning block stays hidden for the remote engine.
    expect(w.text()).not.toContain('Auto memory')
  })

  it('locks the whole section when the engine key is managed', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useSettingsStore()
    settings.server = baseServer(['memory-engine'])
    settings.memoryEngine = 'remote'
    const w = mountSection(pinia)
    const engine = w.findAll('select.field')[0]
    expect((engine.element as HTMLSelectElement).disabled).toBe(true)
  })

  it('shows the ladym tuning fields for the local engine', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useSettingsStore()
    settings.server = baseServer()
    settings.memoryEngine = 'ladym'
    const w = mountSection(pinia)
    expect(w.text()).toContain('Auto memory')
  })
})
