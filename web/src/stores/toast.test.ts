import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useToastStore } from './toast'

describe('useToastStore', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
  })
  afterEach(() => vi.useRealTimers())

  it('errors land as toasts and expire', () => {
    const s = useToastStore()
    s.error(new Error('boom'))
    expect(s.toasts).toHaveLength(1)
    expect(s.toasts[0]).toMatchObject({ message: 'boom', kind: 'error' })

    vi.advanceTimersByTime(5000)
    expect(s.toasts).toHaveLength(0)
  })

  it('stringifies non-Error throws and dismiss removes early', () => {
    const s = useToastStore()
    s.error('plain')
    expect(s.toasts[0].message).toBe('plain')

    const id = s.toasts[0].id
    s.dismiss(id)
    expect(s.toasts).toHaveLength(0)
    // Idempotent: dismissing again is a no-op.
    expect(() => s.dismiss(id)).not.toThrow()
  })

  it('exposes success/warn levels with optional actions', () => {
    const s = useToastStore()
    const action = { label: 'Retry', run: () => {} }
    s.success('done', action)
    s.warn('careful')
    expect(s.toasts[0]).toMatchObject({ message: 'done', kind: 'success', action })
    expect(s.toasts[1]).toMatchObject({ message: 'careful', kind: 'warn' })
  })

  it('caps the stack at three, dropping the oldest', () => {
    const s = useToastStore()
    s.push('one', 'info', 60_000)
    s.push('two', 'info', 60_000)
    s.push('three', 'info', 60_000)
    s.push('four', 'info', 60_000)
    expect(s.toasts.map((t) => t.message)).toEqual(['two', 'three', 'four'])
  })
})
