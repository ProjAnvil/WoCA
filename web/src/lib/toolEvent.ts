// One-line summary of a tool event for the streaming liveness bar: the
// tool name plus its most identifying argument (a path, command, pattern…),
// truncated to keep the bar a single line.
const PREFERRED_KEYS = ['file', 'path', 'cmd', 'command', 'pattern', 'url', 'query', 'target', 'name'] as const

function truncate(s: string): string {
  const first = s.split('\n')[0].trim()
  return first.length > 48 ? `${first.slice(0, 45)}…` : first
}

export function toolEventSummary(name: string, args: unknown): string {
  const a = (args ?? {}) as Record<string, unknown>
  for (const k of PREFERRED_KEYS) {
    const v = a[k]
    if (typeof v === 'string' && v.trim()) return `${name} · ${truncate(v)}`
  }
  for (const v of Object.values(a)) {
    if (typeof v === 'string' && v.trim()) return `${name} · ${truncate(v)}`
  }
  return name
}
