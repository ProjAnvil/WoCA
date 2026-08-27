import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WSheet from './WSheet.vue'

// happy-dom quirk: Vue's `.self` guard compares e.target with the element,
// and a click dispatched on the element itself leaves target unset unless
// pinned explicitly — do that instead of relying on .click().
function clickSelf(el: HTMLElement) {
  const ev = new MouseEvent('click', { bubbles: true, cancelable: true })
  Object.defineProperty(ev, 'target', { value: el })
  el.dispatchEvent(ev)
}

describe('WSheet', () => {
  it('renders as a right-side dialog with dialog semantics', async () => {
    const w = mount(WSheet, {
      props: { title: 'Settings' },
      slots: { default: '<button>inner</button>' },
      attachTo: document.body,
    })
    await flushPromises() // onMounted focuses after the mount flush
    const sheet = document.querySelector('.w-sheet')
    expect(sheet).not.toBeNull()
    expect(sheet?.getAttribute('role')).toBe('dialog')
    expect(sheet?.getAttribute('aria-modal')).toBe('true')
    expect(sheet?.getAttribute('aria-label')).toBe('Settings')
    // Focus moved inside on mount (first focusable).
    expect(sheet?.contains(document.activeElement)).toBe(true)
    w.unmount()
  })

  it('closes on Escape and backdrop clicks', async () => {
    const w = mount(WSheet, {
      slots: { default: '<button>inner</button>' },
      attachTo: document.body,
    })
    await flushPromises()
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    expect(w.emitted('close')).toHaveLength(1)

    clickSelf(document.querySelector('.w-sheet-backdrop') as HTMLElement)
    expect(w.emitted('close')).toHaveLength(2)
    w.unmount()
  })
})
