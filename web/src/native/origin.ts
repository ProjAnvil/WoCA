const loopbackHTTPHosts = new Set(['localhost', '127.0.0.1', '::1'])

function hasValidPort(authority: string): boolean {
  if (authority.startsWith('[')) {
    const closingBracket = authority.lastIndexOf(']')
    if (closingBracket < 0) return false
    if (closingBracket === authority.length - 1) return true
    if (authority[closingBracket + 1] !== ':') return false
    return isValidPort(authority.slice(closingBracket + 2))
  }

  const portStart = authority.lastIndexOf(':')
  return portStart < 0 || isValidPort(authority.slice(portStart + 1))
}

function isValidPort(port: string): boolean {
  return /^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535
}

// This is client-side feedback only. The native config store remains the
// authority that validates and canonicalizes the server origin before saving.
export function validateEnterpriseOrigin(rawOrigin: string): string | null {
  const origin = rawOrigin.trim()
  if (!/^https?:\/\//i.test(origin) || origin.includes('?') || origin.includes('#')) {
    return null
  }

  try {
    const parsed = new URL(origin)
    if (!parsed.host || parsed.username || parsed.password || parsed.pathname !== '/') {
      return null
    }

    const authority = origin.slice(origin.indexOf('//') + 2).replace(/\/$/, '')
    if (authority.includes('/') || !hasValidPort(authority)) {
      return null
    }

    const scheme = parsed.protocol.toLowerCase()
    if (scheme === 'https:') return origin
    if (scheme !== 'http:') return null

    const host = parsed.hostname.toLowerCase().replace(/^\[|\]$/g, '')
    return loopbackHTTPHosts.has(host) ? origin : null
  } catch {
    return null
  }
}
