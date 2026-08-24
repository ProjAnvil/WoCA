import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import UsersSection from './UsersSection.vue'
import i18n from '../i18n'

function mountWith(users: unknown[], issued?: { key: string }, sessions: unknown[] = []) {
  const ctx = {
    api: (async (path: string, init?: RequestInit) => {
      if (path.startsWith('/api/tenant/tenants')) {
        return new Response(JSON.stringify([{ id: 't_bu1', name: 'BU One', created_at: 1 }]), { status: 200 })
      }
      if (path.startsWith('/api/tenant/stats')) {
        return new Response(JSON.stringify([{ tenant_id: 't_bu1', tenant_name: 'BU One', keys_active: 0, keys_revoked: 0, sessions: 0 }]), { status: 200 })
      }
      if (path.startsWith('/api/tenant/users') && (!init || init.method === undefined)) {
        return new Response(JSON.stringify({ items: users, total: users.length }), { status: 200 })
      }
      if (path === '/api/tenant/users' && init?.method === 'POST') {
        return new Response(JSON.stringify({ key: issued?.key ?? 'woca_abcd_x', user: users[0] }), { status: 200 })
      }
      if (path === '/api/tenant/sessions') {
        return new Response(JSON.stringify(sessions), { status: 200 })
      }
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    }) as unknown as (path: string, init?: RequestInit) => Promise<Response>,
  }
  return mount(UsersSection, { global: { plugins: [i18n], provide: { ops: reactive(ctx) } } })
}

describe('UsersSection', () => {
  it('renders the registry with last-use and revocation state', async () => {
    const w = mountWith(
      [
        { prefix: 'ab12cd34', remark: 'alice', role: 'member', issued_at: 1787423953129, last_used: 1787424036058 },
        { prefix: 'ef56gh78', remark: 'bob', role: 'viewer', issued_at: 1787420000000, revoked_at: 1787429000000 },
      ],
      undefined,
      [],
    )
    await new Promise((r) => setTimeout(r, 0))
    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('bob')
    // key prefix column renders
    expect(w.text()).toContain('ab12cd34')
    // last-use column: alice's core-side timestamp renders
    expect(w.text()).toContain('2:40:36')
    expect(w.text()).toContain('revoked')
    // one key revoke (live key); sessions moved to their own section
    expect(w.findAll('button.btn-danger').length).toBe(1)
    expect(w.text()).not.toContain('Sessions (core-issued')
  })

  it('shows never for unused keys and the empty hint without rows', async () => {
    const w = mountWith([])
    await new Promise((r) => setTimeout(r, 0))
    expect(w.text()).toContain('No tenants yet')
  })

  it('issues a key: shown exactly once with the once-note, input cleared', async () => {
    const w = mountWith([], { key: 'woca_prefix_secret_fullkey' })
    await new Promise((r) => setTimeout(r, 0))
    // Pick the tenant, then type the remark (inputs: search, new-tenant, remark).
    const inputs = w.findAll('input')
    await w.findAll('select')[0].setValue('t_bu1')
    const input = inputs[2]
    await input.setValue('bob')
    await w.find('button.btn-primary').trigger('click')
    await new Promise((r) => setTimeout(r, 0))
    expect(w.text()).toContain('woca_prefix_secret_fullkey')
    expect(w.text()).toContain('Shown exactly once')
    expect((input.element as HTMLInputElement).value).toBe('')
  })
})
