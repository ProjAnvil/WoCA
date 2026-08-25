import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

import MessagePlaneSection from './MessagePlaneSection.vue'
import { useServerSettingsStore } from '../../stores/serverSettings'
import type { AppSettings } from '../../types'

function mountSection(pinia: ReturnType<typeof createPinia>) {
  return mount(MessagePlaneSection, { global: { plugins: [i18n, createPinia()] } })
}

describe('MessagePlaneSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
  })

  it('explains the plane is unavailable on non-dispatching deployments', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useServerSettingsStore()
    settings.server = { managed_fields: [], msg_plane_available: false } as unknown as AppSettings
    const w = mountSection(pinia)
    expect(w.text()).toContain('Enterprise-only')
    // The controls stay rendered (bound to store drafts) but the note is
    // the operative signal.
    expect(w.findAll('select.field')).toHaveLength(1)
  })

  it('shows the operating notes on a dispatching deployment', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const settings = useServerSettingsStore()
    settings.server = { managed_fields: [], msg_plane_available: true } as unknown as AppSettings
    const w = mountSection(pinia)
    expect(w.text()).toContain('PG queue')
  })
})
