import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'

import ProviderSection from './ProviderSection.vue'

function mountSection(locked?: boolean) {
  return mount(ProviderSection, {
    props: { locked },
    global: { plugins: [i18n] },
  })
}

describe('ProviderSection', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    setActivePinia(createPinia())
  })

  it('keeps the provider fields editable on un-managed deployments', () => {
    const w = mountSection(false)
    expect(w.find('.mem-intro').exists()).toBe(false)
    for (const field of w.findAll('input.field, select.field')) {
      expect((field.element as HTMLInputElement).disabled).toBe(false)
    }
  })

  it('grays the whole domain with the managed notice in the bridge form', async () => {
    const w = mountSection(true)
    expect(w.find('.mem-intro').text()).toContain('organization')
    for (const field of w.findAll('input.field, select.field')) {
      expect((field.element as HTMLInputElement).disabled).toBe(true)
    }
  })
})
