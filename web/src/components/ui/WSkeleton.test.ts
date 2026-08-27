import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import WSkeleton from './WSkeleton.vue'

describe('WSkeleton', () => {
  it('renders the requested number of decorative shapes', () => {
    const w = mount(WSkeleton, { props: { variant: 'text', repeat: 3 } })
    expect(w.findAll('.w-skeleton-shape')).toHaveLength(3)
    expect(w.attributes('aria-hidden')).toBe('true')
  })

  it('applies explicit sizing and per-variant classes', () => {
    const w = mount(WSkeleton, { props: { variant: 'circle', width: '28px', height: '28px' } })
    const shape = w.get('.w-skeleton-shape')
    expect(shape.classes()).toContain('w-skeleton-circle')
    const el = shape.element as HTMLElement
    expect(el.style.width).toBe('28px')
    expect(el.style.height).toBe('28px')
  })

  it('paragraph variant tapers the last line', () => {
    const w = mount(WSkeleton, { props: { variant: 'paragraph', repeat: 3 } })
    expect(w.classes()).toContain('w-skeleton-paragraph')
    expect(w.findAll('.w-skeleton-shape')).toHaveLength(3)
  })
})
