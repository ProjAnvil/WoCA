export type GreetingSlot = 'morning' | 'afternoon' | 'evening' | 'lateNight'

/** Time-of-day bucket for the empty-session greeting: 05–11 morning,
 * 12–17 afternoon, 18–21 evening, else late night (local time). */
export function greetingSlot(d = new Date()): GreetingSlot {
  const h = d.getHours()
  if (h >= 5 && h < 12) return 'morning'
  if (h >= 12 && h < 18) return 'afternoon'
  if (h >= 18 && h < 22) return 'evening'
  return 'lateNight'
}
