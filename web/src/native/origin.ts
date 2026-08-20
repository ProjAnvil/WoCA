interface RawOrigin {
  origin: string
  scheme: string
  host: string
  bracketedHost: boolean
}

function isValidPort(port: string | undefined): boolean {
  return port === undefined || (/^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535)
}

function isValidNonBracketedHost(host: string): boolean {
  return !/[\x22\x3c\x3e\x5e\x60\x7b\x7d\x7c]/.test(host)
}

function isCanonicalIPv4(host: string): boolean {
  const octets = host.split('.')
  return (
    octets.length === 4 &&
    octets.every((octet) => /^\d+$/.test(octet) && Number(octet) >= 0 && Number(octet) <= 255)
  )
}

function isAlternateIPv4(rawHost: string, parsedHost: string): boolean {
  const canonicalHost = parsedHost.toLowerCase().replace(/^\[|\]$/g, '')
  return rawHost.toLowerCase() !== canonicalHost && isCanonicalIPv4(canonicalHost)
}

function parseRawOrigin(rawOrigin: string): RawOrigin | null {
  const origin = rawOrigin.trim()
  if (/[\u0000-\u001F\u007F\\]/.test(origin)) return null

  const match = /^(https?):\/\/(.*)$/i.exec(origin)
  if (!match) return null

  let authority = match[2]
  if (authority.includes('@') || authority.includes('?') || authority.includes('#') || authority.includes('%')) {
    return null
  }
  if (authority.endsWith('/')) authority = authority.slice(0, -1)
  if (!authority || authority.includes('/')) return null

  if (authority.startsWith('[')) {
    const closingBracket = authority.indexOf(']')
    if (closingBracket < 2) return null
    const rest = authority.slice(closingBracket + 1)
    if (rest !== '' && (!rest.startsWith(':') || !isValidPort(rest.slice(1)))) return null
    return {
      origin,
      scheme: match[1].toLowerCase(),
      host: authority.slice(1, closingBracket),
      bracketedHost: true,
    }
  }

  if (authority.includes('[') || authority.includes(']')) return null
  const firstColon = authority.indexOf(':')
  const lastColon = authority.lastIndexOf(':')
  if (firstColon !== lastColon) return null

  const host = firstColon < 0 ? authority : authority.slice(0, firstColon)
  const port = firstColon < 0 ? undefined : authority.slice(firstColon + 1)
  if (!host || !isValidPort(port) || !isValidNonBracketedHost(host)) return null
  return { origin, scheme: match[1].toLowerCase(), host, bracketedHost: false }
}

// This is client-side feedback only. The native config store remains the
// authority that validates and canonicalizes the server origin before saving.
export function validateEnterpriseOrigin(rawOrigin: string): string | null {
  const raw = parseRawOrigin(rawOrigin)
  if (!raw) return null

  try {
    const parsed = new URL(raw.origin)
    if (
      parsed.protocol.toLowerCase() !== raw.scheme + ':' ||
      !parsed.host ||
      parsed.username ||
      parsed.password ||
      parsed.pathname !== '/' ||
      parsed.search ||
      parsed.hash
    ) {
      return null
    }
    if (!raw.bracketedHost && isAlternateIPv4(raw.host, parsed.hostname)) return null

    if (raw.scheme === 'https') return raw.origin
    const normalizedHost = parsed.hostname.toLowerCase().replace(/^\[|\]$/g, '')
    if (raw.bracketedHost) return normalizedHost === '::1' ? raw.origin : null

    const rawHost = raw.host.toLowerCase()
    return rawHost === 'localhost' || rawHost === '127.0.0.1' ? raw.origin : null
  } catch {
    return null
  }
}
