import { describe, expect, it } from 'vitest'
import { fuzzyScore } from './fuzzy'

describe('fuzzyScore', () => {
  it('empty query matches everything at rank zero', () => {
    expect(fuzzyScore('', 'anything')).toBe(0)
    expect(fuzzyScore('   ', 'anything')).toBe(0)
  })

  it('substring beats subsequence and earlier substrings rank higher', () => {
    const sub = fuzzyScore('set', 'open settings')!
    const seq = fuzzyScore('stg', 'settings')!
    expect(sub).toBeGreaterThan(seq)
    expect(fuzzyScore('settings', 'open settings')!).toBeLessThan(fuzzyScore('settings', 'settings')!)
  })

  it('is case-insensitive', () => {
    expect(fuzzyScore('SET', 'settings')).not.toBeNull()
    expect(fuzzyScore('cmd', 'CMD palette')).not.toBeNull()
  })

  it('adjacent characters score better than scattered ones', () => {
    expect(fuzzyScore('abc', 'xxabcxx')!).toBeGreaterThan(fuzzyScore('abc', 'axbxcx')!)
  })

  it('unmatchable queries return null', () => {
    expect(fuzzyScore('zzz', 'settings')).toBeNull()
    expect(fuzzyScore('mode', 'panels')).toBeNull()
  })
})
