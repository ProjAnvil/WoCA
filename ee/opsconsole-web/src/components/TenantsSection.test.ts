import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import TenantsSection from './TenantsSection.vue'
import i18n from '../i18n'

// The section is exercised through its real wire contract: tenants +
// stats on load, create and the terminal batch delete (the response's
// deleted/blocked/missing verdict drives the toasts).
const calls: { method: string; path: string; body?: unknown }[] = []

function mountWith() {
  calls.length = 0
  const ctx = {
    api: (async (path: string, init?: RequestInit) => {
      calls.push({ method: init?.method ?? 'GET', path, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (path === '/api/tenant/tenants' && (!init || init.method === undefined)) {
        return new Response(JSON.stringify([
          { id: 'local', name: 'local machine', created_at: 1787000000000 },
          { id: 't_bu1', name: 'BU One', created_at: 1787100000000 },
        ]), { status: 200 })
      }
      if (path === '/api/tenant/stats') {
        return new Response(JSON.stringify([
          { tenant_id: 'local', tenant_name: 'local machine', keys_active: 0, keys_revoked: 0, sessions: 0 },
          { tenant_id: 't_bu1', tenant_name: 'BU One', keys_active: 2, keys_revoked: 1, sessions: 3, last_used: 1787424036058 },
        ]), { status: 200 })
      }
      if (path === '/api/tenant/tenants' && init?.method === 'POST') {
        return new Response(JSON.stringify({ id: 't_new', name: 'BU Two', created_at: 1 }), { status: 200 })
      }
      if (path === '/api/tenant/tenants/delete') {
        return new Response(JSON.stringify({ ok: true, deleted: ['t_bu1'], blocked: [], missing: [] }), { status: 200 })
      }
      return new Response(JSON.stringify({ ok: true }), { status: 200 })
    }) as unknown as (path: string, init?: RequestInit) => Promise<Response>,
  }
  return mount(TenantsSection, { global: { plugins: [i18n], provide: { ops: reactive(ctx) } } })
}

async function settle() {
  await new Promise((r) => setTimeout(r, 0))
}

describe('TenantsSection', () => {
  it('renders the registry with scale columns; the platform tenant is locked', async () => {
    const w = mountWith()
    await settle()
    expect(w.text()).toContain('BU One')
    expect(w.text()).toContain('t_bu1')
    // scale columns come from /api/tenant/stats
    expect(w.text()).toContain('2')
    // the locked row carries the lock hint instead of a checkbox
    expect(w.find('.ops-lock').exists()).toBe(true)
    const checks = w.findAll('tbody input[type="checkbox"]')
    expect(checks.length).toBe(1)
  })

  it('creates a tenant through the dialog, not an inline form', async () => {
    const w = mountWith()
    await settle()
    await w.findAll('button').find((b) => b.text().includes('New tenant'))!.trigger('click')
    expect(w.find('.modal-open').exists()).toBe(true)
    const modal = w.find('.modal-box')
    await modal.find('input').setValue('BU Two')
    await modal.findAll('button').find((b) => b.text() === 'Create')!.trigger('click')
    await settle()
    const post = calls.find((c) => c.path === '/api/tenant/tenants' && c.method === 'POST')
    expect(post?.body).toEqual({ name: 'BU Two' })
    // dialog closes on success
    expect(w.find('.modal-open').exists()).toBe(false)
  })

  it('batch-deletes the selection with a confirm dialog and a clear verdict', async () => {
    const w = mountWith()
    await settle()
    await w.find('tbody input[type="checkbox"]').setValue(true)
    // the batch bar replaces the toolbar
    expect(w.text()).toContain('1 selected')
    await w.findAll('button').find((b) => b.text().includes('Delete selected'))!.trigger('click')
    const modal = w.find('.modal-box')
    expect(modal.text()).toContain('This deletes 1 tenants')
    await modal.findAll('button').find((b) => b.text().includes('Delete'))!.trigger('click')
    await settle()
    const del = calls.find((c) => c.path === '/api/tenant/tenants/delete')
    expect(del?.body).toEqual({ ids: ['t_bu1'] })
    expect(w.find('.modal-open').exists()).toBe(false)
  })
})
