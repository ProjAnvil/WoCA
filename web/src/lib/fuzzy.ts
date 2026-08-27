/**
 * Tiny fuzzy scorer for the command palette: substring matches win (earlier
 * and shorter targets rank higher); otherwise a subsequence match with an
 * adjacency bonus. Returns null when the query cannot be matched at all.
 */
export function fuzzyScore(query: string, target: string): number | null {
  const q = query.trim().toLowerCase()
  const t = target.toLowerCase()
  if (!q) return 0

  const idx = t.indexOf(q)
  if (idx >= 0) return 1000 - idx - t.length * 0.01

  let ti = 0
  let score = 0
  let streak = 0
  for (const ch of q) {
    if (ch === ' ') continue
    const found = t.indexOf(ch, ti)
    if (found < 0) return null
    streak = found === ti ? streak + 1 : 0
    score += 10 + streak * 5 - Math.min(found - ti, 10)
    ti = found + 1
  }
  return score
}
