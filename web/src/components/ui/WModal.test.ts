import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WModal from './WModal.vue'

describe('WModal', () => {
  it('teleports the dialog to body only while open', async () => {
    const w = mount(WModal, {
      props: { open: false, title: 'Demo' },
      slots: { default: '<button>inner</button>' },
      attachTo: document.body,
    })
    expect(document.querySelector('.w-modal-panel')).toBeNull()
    await w.setProps({ open: true })
    await flushPromises()
    const panel = document.querySelector('.w-modal-panel')
    expect(panel).not.toBeNull()
    expect(panel?.getAttribute('role')).toBe('dialog')
    expect(panel?.getAttribute('aria-modal')).toBe('true')
    expect(panel?.getAttribute('aria-label')).toBe('Demo')
    // Open moves focus inside the dialog (first focusable).
    expect(panel?.contains(document.activeElement)).toBe(true)
    w.unmount()
  })

  it('closes on Escape via the close event', async () => {
    const w = mount(WModal, {
      props: { open: true },
      slots: { default: '<button>inner</button>' },
      attachTo: document.body,
    })
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }),
    )
    expect(w.emitted('close')).toHaveLength(1)
    w.unmount()
  })

  it('persistent modals swallow Escape', async () => {
    const w = mount(WModal, {
      props: { open: true, persistent: true },
      slots: { default: '<button>inner</button>' },
      attachTo: document.body,
    })
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }),
    )
    expect(w.emitted('close')).toBeUndefined()
    w.unmount()
  })

  it('traps Tab focus inside the panel', async () => {
    const w = mount(WModal, {
      props: { open: true },
      slots: { default: '<button id="a">a</button><button id="b">b</button>' },
      attachTo: document.body,
    })
    const last = document.getElementById('b')!
    const first = document.getElementById('a')!
    last.focus()
    last.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }))
    expect(document.activeElement).toBe(first)
    w.unmount()
  })
})
