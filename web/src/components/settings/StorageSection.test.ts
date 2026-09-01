import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

import StorageSection from './StorageSection.vue'
import { useServerSettingsStore } from '../../stores/serverSettings'
import type { AppSettings } from '../../types'

function baseServer(over: Partial<AppSettings> = {}): AppSettings {
  return {
    managed_fields: [],
    msg_plane_available: false,
    ...over,
  } as AppSettings
}

function mountSection(pinia: ReturnType<typeof createPinia>, bridge?: boolean) {
  return mount(StorageSection, {
    props: { bridge },
    global: { plugins: [i18n, pinia] },
  })
}

describe('StorageSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
  })

  it('keeps the backends editable when un-managed', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useServerSettingsStore()
    settings.server = baseServer()
    const w = mountSection(pinia)
    const selects = w.findAll('select.field')
    expect(selects).toHaveLength(2)
    for (const s of selects) expect((s.element as HTMLSelectElement).disabled).toBe(false)
    // The DSN input exists where a DSN is a user decision.
    expect(w.find('input[type="password"]').exists()).toBe(true)
  })

  it('locks the domain and hides the DSN input on managed bridges', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useServerSettingsStore()
    settings.server = baseServer({
      managed_fields: ['checkpoint', 'memory'],
    })
    settings.checkpointBackend = 'postgres'
    settings.memoryBackend = 'postgres'
    const w = mountSection(pinia, true)
    expect(w.find('.mem-intro').text()).toContain('organization')
    for (const s of w.findAll('select.field')) {
      expect((s.element as HTMLSelectElement).disabled).toBe(true)
    }
    // Coordinates ride the config package on managed bridges — no local
    // DSN field to mislead anyone with.
    expect(w.find('input[type="password"]').exists()).toBe(false)
  })
})
