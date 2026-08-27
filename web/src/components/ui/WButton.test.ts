import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import WButton from './WButton.vue'

describe('WButton', () => {
  it('renders variant and size classes', () => {
    const w = mount(WButton, { props: { variant: 'danger', size: 'lg' }, slots: { default: 'Go' } })
    expect(w.classes()).toContain('w-btn--danger')
    expect(w.classes()).toContain('w-btn--lg')
    expect(w.text()).toBe('Go')
  })

  it('loading disables and flags aria-busy without dropping the label', async () => {
    const w = mount(WButton, { props: { loading: false }, slots: { default: 'Save' } })
    expect(w.attributes('aria-busy')).toBeUndefined()
    await w.setProps({ loading: true })
    expect(w.attributes('disabled')).toBeDefined()
    expect(w.attributes('aria-busy')).toBe('true')
    expect(w.text()).toBe('Save')
  })

  it('block mode spans the container', () => {
    const w = mount(WButton, { props: { block: true } })
    expect(w.classes()).toContain('w-btn--block')
  })
})
