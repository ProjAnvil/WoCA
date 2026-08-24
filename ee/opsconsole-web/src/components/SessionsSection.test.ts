import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import SessionsSection from './SessionsSection.vue'
import i18n from '../i18n'

function mountWith(sessions: unknown[]) {
  const ctx = {
    api: (async (path: string, init?: RequestInit) => {
      if (path.startsWith('/api/tenant/sessions') && (!init || init.method === undefined)) {
        return new Response(JSON.stringify({ items: sessions, total: sessions.length }), { status: 200 })
      }
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    }) as unknown as (path: string, init?: RequestInit) => Promise<Response>,
  }
  return mount(SessionsSection, { global: { plugins: [i18n], provide: { ops: reactive(ctx) } } })
}

describe('SessionsSection', () => {
  it('renders the login-state inventory with per-session revoke on live rows only', async () => {
    const w = mountWith([
      { token_hash: 'a'.repeat(64), key_prefix: 'ab12cd34', remark: 'alice', role: 'member', created_at: 1, last_seen: 1787424036058, expires_at: 1788028836058 },
      { token_hash: 'b'.repeat(64), key_prefix: 'ef56gh78', remark: 'bob', role: 'viewer', created_at: 1, last_seen: 1787424036058, expires_at: 1788028836058, revoked_at: 1787429000000 },
    ])
    await new Promise((r) => setTimeout(r, 0))
    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('ab12cd34')
    expect(w.text()).toContain('revoked')
    expect(w.findAll('button.btn-danger').length).toBe(1)
    expect(w.text()).toContain('Each row is one login')
  })

  it('shows the empty hint without sessions', async () => {
    const w = mountWith([])
    await new Promise((r) => setTimeout(r, 0))
    expect(w.text()).toContain('No sign-in sessions.')
  })
})
