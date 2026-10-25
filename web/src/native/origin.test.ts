import { describe, expect, it } from 'vitest'

import { validateEnterpriseOrigin } from './origin'

describe('validateEnterpriseOrigin', () => {
  it.each([
    ['remote HTTP', 'http://enterprise.example.com'],
    ['relative URL', '/enterprise'],
    ['path', 'https://enterprise.example.com/app'],
    ['credentials', 'https://user:secret@enterprise.example.com'],
    ['query', 'https://enterprise.example.com?tenant=acme'],
    ['fragment', 'https://enterprise.example.com#login'],
    ['invalid port', 'https://enterprise.example.com:65536'],
    ['zero port', 'https://enterprise.example.com:0'],
    ['empty port', 'https://enterprise.example.com:'],
    ['empty userinfo', 'https://@enterprise.example.com'],
    ['encoded userinfo', 'https://%75ser@enterprise.example.com'],
    ['encoded at-sign userinfo', 'https://%40@enterprise.example.com'],
    ['backslash', 'https://enterprise.example.com\\'],
    ['tab control character', 'https://enterprise.\texample.com'],
    ['NUL control character', 'https://enterprise.example.com\u0000'],
    ['abbreviated IPv4 loopback', 'http://127.1'],
    ['decimal IPv4 loopback', 'http://2130706433'],
    ['hex IPv4 loopback', 'http://0x7f000001'],
    ['octal-like IPv4 loopback', 'http://0177.0.0.1'],
    ['bracketed non-IPv6 host', 'http://[localhost]:8080'],
    ['IPv6 zone', 'http://[fe80::1%25en0]:8080'],
    ['ambiguous authority', 'https://enterprise.example.com::443'],
    ['backtick host delimiter', 'https://exa`mple.com'],
    ['left brace host delimiter', 'https://exa{mple.com'],
    ['right brace host delimiter', 'https://exa}mple.com'],
    ['quote host delimiter', 'https://exa"mple.com'],
    ['left angle host delimiter', 'https://exa<mple.com'],
    ['right angle host delimiter', 'https://exa>mple.com'],
    ['caret host delimiter', 'https://exa^mple.com'],
    ['pipe host delimiter', 'https://exa|mple.com'],
    ['HTTPS abbreviated IPv4', 'https://127.1'],
    ['HTTPS decimal IPv4', 'https://2130706433'],
    ['HTTPS hexadecimal IPv4', 'https://0x7f000001'],
    ['HTTPS octal-like IPv4', 'https://0177.0.0.1'],
    ['soft hyphen in hostname', 'https://exa\u00admple.com'],
    ['zero-width space in hostname', 'https://exa\u200bmple.com'],
    ['BOM in hostname', 'https://exa\ufeffmple.com'],
    ['BOM adjacent to authority port', 'https://enterprise.example.com:8443\ufeff'],
  ])('rejects %s', (_name, rawOrigin) => {
    expect(validateEnterpriseOrigin(rawOrigin)).toBeNull()
  })

  it.each([
    ['HTTPS origin', '  https://enterprise.example.com:8443/  ', 'https://enterprise.example.com:8443/'],
    ['HTTPS default port and DNS case', 'HTTPS://EXAMPLE.COM:443/', 'HTTPS://EXAMPLE.COM:443/'],
    ['HTTPS canonical dotted IPv4', 'https://127.0.0.1:8443', 'https://127.0.0.1:8443'],
    ['HTTPS Unicode IDN', 'https://exämple.test', 'https://exämple.test'],
    ['HTTPS punycode IDN', 'https://xn--fsqu00a.xn--0zwm56d', 'https://xn--fsqu00a.xn--0zwm56d'],
    ['HTTPS trailing-dot DNS', 'https://enterprise.example.com.', 'https://enterprise.example.com.'],
    ['HTTPS numeric DNS label', 'https://123.example.com', 'https://123.example.com'],
    ['localhost HTTP', 'http://localhost:8080', 'http://localhost:8080'],
    ['localhost HTTP case', 'http://LOCALHOST:8080', 'http://LOCALHOST:8080'],
    ['IPv4 loopback HTTP', 'http://127.0.0.1:8080', 'http://127.0.0.1:8080'],
    ['IPv6 loopback HTTP without port', 'http://[::1]', 'http://[::1]'],
    ['IPv6 loopback HTTP', 'http://[::1]:8080', 'http://[::1]:8080'],
    ['equivalent IPv6 loopback HTTP', 'http://[0:0:0:0:0:0:0:1]:8080', 'http://[0:0:0:0:0:0:0:1]:8080'],
  ])('accepts %s', (_name, rawOrigin, want) => {
    expect(validateEnterpriseOrigin(rawOrigin)).toBe(want)
  })
})
