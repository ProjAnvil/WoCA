// Shared pure formatting helpers (previously duplicated across stores and
// views — keep them side-effect free so any module can import them).

// Token counts stay integral when they divide evenly, otherwise one decimal
// (1.5k, 2.3M); small counts render verbatim.
/** 61_000 → "1m 1s"; 3_740_000 → "1h 2m"; under a minute → "42s". */
export function fmtDuration(ms: number): string {
  const sec = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = sec % 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

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

/** 512 → "512 B"; 4096 → "4 KB"; 1_500_000 → "1.5 MB" (output tails). */
export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

/** ISO timestamp → short local "Nov 7, 14:05" (cron fire times). */
export function fmtDateTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}
