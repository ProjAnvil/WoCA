import { describe, expect, it } from 'vitest'
import { greetingSlot } from './greeting'

const at = (h: number) => new Date(2026, 9, 6, h, 0)

describe('greetingSlot', () => {
  it.each([5, 8, 11])('%i:00 → morning', (h) => expect(greetingSlot(at(h))).toBe('morning'))
  it.each([12, 15, 17])('%i:00 → afternoon', (h) => expect(greetingSlot(at(h))).toBe('afternoon'))
  it.each([18, 20, 21])('%i:00 → evening', (h) => expect(greetingSlot(at(h))).toBe('evening'))
  it.each([22, 23, 0, 3, 4])('%i:00 → lateNight', (h) => expect(greetingSlot(at(h))).toBe('lateNight'))
})
