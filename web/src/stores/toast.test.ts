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
})
