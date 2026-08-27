import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useConfirmStore } from './confirm'

describe('useConfirmStore', () => {
  beforeEach(() => setActivePinia(createPinia()))

  it('ask exposes the request and resolves true on confirm', async () => {
    const s = useConfirmStore()
    const p = s.ask({
      title: 'Delete?',
      confirmLabel: 'Delete',
      cancelLabel: 'Cancel',
      tone: 'danger',
    })
    expect(s.req).toMatchObject({ title: 'Delete?', tone: 'danger' })
    s.settle(true)
    await expect(p).resolves.toBe(true)
    expect(s.req).toBeNull()
  })

  it('cancel resolves false and clears the request', async () => {
    const s = useConfirmStore()
    const p = s.ask({ title: 't', confirmLabel: 'c', cancelLabel: 'x' })
    s.settle(false)
    await expect(p).resolves.toBe(false)
    expect(s.req).toBeNull()
  })

  it('settle without an active ask is a no-op', () => {
    const s = useConfirmStore()
    expect(() => s.settle(true)).not.toThrow()
    expect(s.req).toBeNull()
  })
})
