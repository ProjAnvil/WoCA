import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import KeysSection from './KeysSection.vue'
import i18n from '../i18n'

// The section is exercised through its real wire contract: the server-
// paged key list (q/tenant/state/order ride the query string), the issue
// exchange (full key shown exactly once), and the batch revoke.
const calls: { method: string; path: string; body?: unknown }[] = []
const items = [
  { prefix: 'ab12cd34', remark: 'alice', role: 'member', tenant_id: 't_bu1', tenant_name: 'BU One', issued_at: 1787423953129, last_used: 1787424036058 },
  { prefix: 'ef56gh78', remark: 'bob', role: 'viewer', tenant_id: 't_bu1', tenant_name: 'BU One', issued_at: 1787420000000, revoked_at: 1787429000000 },
]

function mountWith() {
  calls.length = 0
  const ctx = {
    api: (async (path: string, init?: RequestInit) => {
      calls.push({ method: init?.method ?? 'GET', path, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (path === '/api/tenant/tenants') {
        return new Response(JSON.stringify([{ id: 't_bu1', name: 'BU One', created_at: 1 }]), { status: 200 })
      }
      if (path.startsWith('/api/tenant/users') && (!init || init.method === undefined)) {
        return new Response(JSON.stringify({ items, total: items.length }), { status: 200 })
      }
      if (path === '/api/tenant/users' && init?.method === 'POST') {
        return new Response(JSON.stringify({ key: 'woca_prefix_secret_fullkey', user: items[0] }), { status: 200 })
      }
      if (path === '/api/tenant/users/revoke') {
        return new Response(JSON.stringify({ ok: true, revoked: 1, requested: 1 }), { status: 200 })
      }
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    }) as unknown as (path: string, init?: RequestInit) => Promise<Response>,
  }
  return mount(KeysSection, { global: { plugins: [i18n], provide: { ops: reactive(ctx) } } })
}

async function settle() {
  await new Promise((r) => setTimeout(r, 0))
}

function modal(w: ReturnType<typeof mountWith>) {
  return w.findAll('.modal-box').at(-1)!
}

describe('KeysSection', () => {
  it('renders the registry; only live keys are selectable', async () => {
    const w = mountWith()
    await settle()
    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('ab12cd34')
    expect(w.text()).toContain('revoked')
    // bob's revoked row has no checkbox
    const checks = w.findAll('tbody input[type="checkbox"]')
    expect(checks.length).toBe(1)
    // the list is server-paged with the full filter surface
    const list = calls.find((c) => c.path.startsWith('/api/tenant/users?'))
    expect(list?.path).toContain('order=issued_desc')
    expect(list?.path).toContain('limit=20')
  })

  it('batch-revokes the selection through a confirm dialog', async () => {
    const w = mountWith()
    await settle()
    await w.find('tbody input[type="checkbox"]').setValue(true)
    expect(w.text()).toContain('1 selected')
    await w.findAll('button').find((b) => b.text().includes('Revoke selected'))!.trigger('click')
    expect(modal(w).text()).toContain('This immediately revokes 1 keys')
    await modal(w).findAll('button').find((b) => b.text().includes('Revoke'))!.trigger('click')
    await settle()
    const revoke = calls.find((c) => c.path === '/api/tenant/users/revoke')
    expect(revoke?.body).toEqual({ prefixes: ['ab12cd34'] })
    expect(w.find('.modal-open').exists()).toBe(false)
  })

  it('issues a key: dialog form → one-time key stage with explicit close', async () => {
    const w = mountWith()
    await settle()
    await w.findAll('button').find((b) => b.text().includes('Issue key'))!.trigger('click')
    // the dialog's first select is the tenant (the toolbar selects are
    // outside the modal box)
    await modal(w).findAll('select')[0].setValue('t_bu1')
    await modal(w).find('input').setValue('carol')
    await modal(w).findAll('button').find((b) => b.text() === 'Issue key')!.trigger('click')
    await settle()
    const issue = calls.find((c) => c.path === '/api/tenant/users' && c.method === 'POST')
    expect(issue?.body).toEqual({ remark: 'carol', role: 'member', tenant_id: 't_bu1' })
    // the one-time key stage shows the full key and cannot be dismissed
    // by accident: only the explicit "saved" button closes it
    expect(w.text()).toContain('woca_prefix_secret_fullkey')
    expect(w.text()).toContain('shown exactly once')
    await w.findAll('button').find((b) => b.text().includes('Saved'))!.trigger('click')
    expect(w.find('.modal-open').exists()).toBe(false)
  })
})
