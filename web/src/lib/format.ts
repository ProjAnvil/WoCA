// Shared pure formatting helpers (previously duplicated across stores and
// views — keep them side-effect free so any module can import them).

// Token counts stay integral when they divide evenly, otherwise one decimal
// (1.5k, 2.3M); small counts render verbatim.
export function fmtTokens(n: number): string {
  if (!n) return '0'
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`
  return String(n)
}

// Trim, then cap at n characters with an ellipsis marker.
export function truncate(s: string, n: number): string {
  const t = s.trim()
  return t.length > n ? t.slice(0, n) + '…' : t
}

// Final path segment ('/a/b/' → 'b'); trailing slashes are ignored.
export function baseName(path: string): string {
  const p = path.replace(/\/+$/, '')
  const i = p.lastIndexOf('/')
  return i >= 0 ? p.slice(i + 1) : p
}
