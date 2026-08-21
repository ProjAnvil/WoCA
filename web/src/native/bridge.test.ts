import { describe, expect, it, vi } from 'vitest'

import { getNativeBridge } from './bridge'

describe('getNativeBridge', () => {
  it('returns null when the Wails bridge is absent', () => {
    const previousGo = window.go
    vi.stubGlobal('go', undefined)

    expect(getNativeBridge()).toBeNull()

    vi.stubGlobal('go', previousGo)
  })
})
