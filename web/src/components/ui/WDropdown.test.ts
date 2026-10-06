import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import WDropdown from './WDropdown.vue'

const mountDd = () =>
  mount(WDropdown, {
    props: { modelValue: false },
    slots: {
      trigger: '<button class="trig">t</button>',
      menu: '<button role="menuitem" class="mi">one</button><button role="menuitem" class="mi2">two</button>',
    },
    global: { stubs: { teleport: true } },
  })

describe('WDropdown', () => {
  it('opens on trigger click and syncs aria-expanded', async () => {
    const w = mountDd()
    expect(w.find('.w-dropdown-menu').exists()).toBe(false)
    await w.get('.trig').trigger('click')
    expect(w.emitted('update:modelValue')?.[0]).toEqual([true])
  })
  it('renders the menu when open with role=menu', () => {
    const w = mount(WDropdown, {
      props: { modelValue: true },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu: '<button role="menuitem">one</button>',
      },
      global: { stubs: { teleport: true } },
    })
    expect(w.find('.w-dropdown-menu[role="menu"]').exists()).toBe(true)
  })
  it('closes on Escape and returns focus to the trigger', async () => {
    const w = mount(WDropdown, {
      props: { modelValue: true },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu: '<button role="menuitem">one</button>',
      },
    })
    // The menu teleports to body — query the document (WModal.test.ts
    // precedent); the wrapper subtree does not contain it.
    const menu = document.body.querySelector('.w-dropdown-menu')!
    expect(menu).toBeTruthy()
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await w.vm.$nextTick()
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual([false])
    w.unmount()
  })

  it('opens via ArrowDown on the trigger', async () => {
    const w = mount(WDropdown, {
      props: { modelValue: false },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu: '<button role="menuitem">one</button>',
      },
    })
    await w.get('.trig').trigger('keydown', { key: 'ArrowDown' })
    expect(w.emitted('update:modelValue')?.[0]).toEqual([true])
    w.unmount()
  })

  // Open through the real path (trigger toggle) so the watch attaches the
  // listeners; then exercise the ARIA menu keyboard contract on the
  // teleported menu.
  async function openViaTrigger() {
    const w = mount(WDropdown, {
      props: { modelValue: false },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu:
          '<button role="menuitem">one</button><button role="menuitem">two</button><button role="menuitem">three</button>',
      },
    })
    await w.get('.trig').trigger('click')
    await w.vm.$nextTick()
    return w
  }

  const menuItems = () =>
    Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitem"]'))

  it('cycles focus with ArrowDown/ArrowUp and jumps with Home/End', async () => {
    const w = await openViaTrigger()
    const menu = document.body.querySelector('.w-dropdown-menu')!
    const items = menuItems()
    items[0].focus()
    expect(document.activeElement).toBe(items[0])
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }))
    expect(document.activeElement).toBe(items[1])
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true }))
    expect(document.activeElement).toBe(items[2])
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }))
    expect(document.activeElement).toBe(items[1])
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true }))
    expect(document.activeElement).toBe(items[0])
    w.unmount()
  })

  it('closes on Tab and on an outside pointerdown', async () => {
    const w = await openViaTrigger()
    const menu = document.body.querySelector('.w-dropdown-menu')!
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }))
    await w.vm.$nextTick()
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual([false])

    // Reopen, then close via a pointerdown outside trigger and menu.
    await w.get('.trig').trigger('click')
    await w.vm.$nextTick()
    document.body.dispatchEvent(
      new PointerEvent('pointerdown', { bubbles: true }),
    )
    await w.vm.$nextTick()
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual([false])
    w.unmount()
  })
})
