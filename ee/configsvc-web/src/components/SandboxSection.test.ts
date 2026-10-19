import { describe, expect, it } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import SandboxSection from './SandboxSection.vue'
import i18n from '../i18n'

// Sandbox backend section: list + register form + verify. The stored key
// never renders — only the key_set marker does.
function mountWith(backends: unknown[]) {
  const calls: Array<{ method?: string; path?: string; body?: unknown }> = []
  const api = async (path: string, init?: RequestInit) => {
    calls.push({ method: init?.method, path, body: init?.body })
    if (path === '/api/sandboxes' && (!init || init.method === undefined)) {
      return new Response(JSON.stringify(backends), { status: 200 })
    }
    return new Response(JSON.stringify({ ok: true }), { status: 200 })
  }
  const w = mount(SandboxSection, {
    global: {
      plugins: [i18n],
      provide: {
        cfg: {
          s: null, d: () => '', v: () => '', setDraft: () => {},
          save: async () => {}, saving: false, savedFlash: '', error: '', api,
        },
      },
    },
  })
  return { w, calls }
}

const sample = {
  id: 'sb-1', name: 'fake', type: 'contract-http', endpoint: 'http://127.0.0.1:9999',
  timeout_ms: 120000, enabled: true, key_set: true, last_probe_at: 1700000000000,
  last_probe_ok: true, last_probe_error: '',
}

describe('SandboxSection', () => {
  it('renders the backend list with type and endpoint', async () => {
    const { w } = mountWith([sample])
    await flushPromises()
    expect(w.text()).toContain('fake')
    expect(w.text()).toContain('contract-http')
    expect(w.text()).toContain('http://127.0.0.1:9999')
  })

  it('never renders the key, only the key_set marker', async () => {
    const { w } = mountWith([{ ...sample, key_set: true }])
    await flushPromises()
    // Open the editor for the stored backend: the password field shows
    // the encrypted-set marker; the fixture carries no key material and
    // none may appear.
    await w.find('.backend-row').trigger('click')
    expect(w.text()).not.toContain('api_key')
    const pw = w.find('input[type="password"]')
    expect(pw.attributes('value')).toBeUndefined()
    expect((pw.element as HTMLInputElement).value).toBe('')
    expect(pw.attributes('placeholder')).toContain('stored encrypted')
  })

  it('offers the full 7-value type enum in the dropdown', async () => {
    const { w } = mountWith([])
    await flushPromises()
    await w.findAll('button').find(b => b.text().includes('New'))!.trigger('click')
    const select = w.find('select')
    const options = select.findAll('option')
    expect(options).toHaveLength(7)
    expect(options.map(o => o.attributes('value'))).toEqual([
      'contract-http', 'e2b', 'daytona', 'modal', 'microsandbox', 'cfbridge', 'sidecar',
    ])
  })

  it('blocks saving until name and endpoint are filled, then POSTs', async () => {
    const { w, calls } = mountWith([])
    await flushPromises()
    await w.findAll('button').find(b => b.text().includes('New'))!.trigger('click')
    const save = w.findAll('button').find(b => b.text().includes('Save'))!
    expect(save.attributes('disabled')).toBeDefined()
    const inputs = w.findAll('input')
    await inputs[0].setValue('fake')
    expect(save.attributes('disabled')).toBeDefined()
    await inputs[1].setValue('http://127.0.0.1:9999')
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click')
    await flushPromises()
    const post = calls.find(c => c.method === 'POST' && c.path === '/api/sandboxes')
    expect(post).toBeTruthy()
    const body = JSON.parse(String(post!.body))
    expect(body.name).toBe('fake')
    expect(body.endpoint).toBe('http://127.0.0.1:9999')
    // The api key only rides the request when the operator typed one.
    expect(body.api_key).toBeUndefined()
  })
})
