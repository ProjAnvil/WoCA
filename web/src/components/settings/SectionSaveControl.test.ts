import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

const api = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/client')>()
  return { ...orig, ...api }
})

import SectionSaveControl from './SectionSaveControl.vue'
import { useServerSettingsStore, type SectionId } from '../../stores/serverSettings'

function mountControl(pinia: ReturnType<typeof createPinia>, section: SectionId) {
  return mount(SectionSaveControl, {
    props: { section },
    global: { plugins: [i18n, pinia] },
  })
}

describe('SectionSaveControl', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
    api.getSettings.mockResolvedValue({ provider: 'anthropic', msg_plane_available: false })
  })

  it('links the sr-only unsaved hint to the save button only while dirty', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const s = useServerSettingsStore()
    await s.loadServer()

    // Clean: no hint element, no describedby linkage on the button.
    const w = mountControl(pinia, 'storage')
    expect(w.find('.sr-only').exists()).toBe(false)
    expect(w.find('[data-testid="section-save"]').attributes('aria-describedby')).toBeUndefined()

    // Dirty: the hint renders with the section-scoped id and the save
    // button points at it, so assistive tech announces the state for the
    // control that acts on it.
    s.checkpointBackend = 'postgres'
    await w.vm.$nextTick()
    const hint = w.find('.sr-only')
    expect(hint.exists()).toBe(true)
    expect(hint.attributes('id')).toBe('storage-dirty-hint')
    expect(w.find('[data-testid="section-save"]').attributes('aria-describedby')).toBe(
      'storage-dirty-hint',
    )
  })
})
