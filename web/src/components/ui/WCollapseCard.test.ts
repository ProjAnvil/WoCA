import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import WCollapseCard from './WCollapseCard.vue'

function mountCard(props: Partial<InstanceType<typeof WCollapseCard>['$props']> = {}) {
  return mount(WCollapseCard, {
    props: { title: 'Read file', open: false, ...props },
    slots: { default: '<p class="body">content</p>', icon: '<span class="ic" />' },
  })
}

describe('WCollapseCard', () => {
  it('renders the header with dialog-semantic expansion state', () => {
    const w = mountCard({ meta: '3 lines' })
    const head = w.get('.w-collapse-head')
    expect(head.attributes('aria-expanded')).toBe('false')
    expect(w.text()).toContain('Read file')
    expect(w.text()).toContain('3 lines')
    // Collapsed body is hidden from the a11y tree and interaction.
    expect(w.get('.w-collapse-body-inner').attributes('aria-hidden')).toBe('true')
  })

  it('toggling emits update:open and reflects the open state', async () => {
    const w = mountCard({ open: true })
    expect(w.get('.w-collapse-head').attributes('aria-expanded')).toBe('true')
    expect(w.get('.w-collapse-body-inner').attributes('aria-hidden')).toBeUndefined()
    expect(w.find('[inert]').exists()).toBe(false)

    await w.get('.w-collapse-head').trigger('click')
    expect(w.emitted('update:open')![0]).toEqual([false])
  })

  it('live cards carry the brand tone and breathing dot', () => {
    const w = mountCard({ live: true, tone: 'brand' })
    expect(w.classes()).toContain('w-collapse-card--brand')
    expect(w.classes()).toContain('live')
    expect(w.find('.w-collapse-live').exists()).toBe(true)
  })
})
