import { describe, expect, it } from 'vitest'

import { en, zhCN } from './locales'

// Locale-shape contract: the settings tree regressed once when new groups
// were inserted into an unclosed `nav` block — the components addressed
// settings.cloud.* while the values lived at settings.nav.cloud.*, and no
// type checker or runtime test caught it (every key rendered as its own
// name). This pins the structural layout both locales must keep.
describe('locale structure', () => {
  it('keeps the cloud-access group at settings.cloud (not nested in nav)', () => {
    for (const [name, locale] of [
      ['zh-CN', zhCN],
      ['en', en],
    ] as const) {
      const settings = locale.settings as Record<string, unknown>
      const nav = settings.nav as Record<string, unknown>
      expect(typeof (settings.cloud as Record<string, unknown>)?.edgeUrl, name).toBe('string')
      expect(typeof (settings.cloud as Record<string, unknown>)?.tunnelServing, name).toBe('string')
      expect(typeof nav?.cloudaccess, name).toBe('string')
      expect(typeof nav?.admin, name).toBe('string')
      expect(nav?.cloud, `${name}: cloud group must not nest inside nav`).toBeUndefined()
    }
  })

  it('keeps zh-CN and en key sets in sync for the cloud and nav groups', () => {
    const zh = zhCN.settings as { cloud: Record<string, unknown>; nav: Record<string, unknown> }
    const enl = en.settings as { cloud: Record<string, unknown>; nav: Record<string, unknown> }
    expect(Object.keys(zh.cloud).sort()).toEqual(Object.keys(enl.cloud).sort())
    expect(Object.keys(zh.nav).sort()).toEqual(Object.keys(enl.nav).sort())
  })
})
