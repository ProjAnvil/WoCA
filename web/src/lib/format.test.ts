import { describe, expect, it } from 'vitest'
import { fmtDuration, fmtTokens, truncate } from './format'

describe('fmtDuration', () => {
  it('formats seconds, minutes, and hours with human rounding', () => {
    expect(fmtDuration(0)).toBe('0s')
    expect(fmtDuration(42_000)).toBe('42s')
    expect(fmtDuration(61_000)).toBe('1m 1s')
    expect(fmtDuration(3_740_000)).toBe('1h 2m')
  })

  it('never goes negative', () => {
    expect(fmtDuration(-5_000)).toBe('0s')
  })
})

describe('fmtTokens / truncate', () => {
  it('compacts token counts', () => {
    expect(fmtTokens(950)).toBe('950')
    expect(fmtTokens(12_400)).toBe('12k')
    expect(fmtTokens(2_500_000)).toBe('2.5M')
  })

  it('truncates with an ellipsis', () => {
    expect(truncate('abcdefgh', 3)).toBe('abc…')
    expect(truncate('ab', 3)).toBe('ab')
  })
})
