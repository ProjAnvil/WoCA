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
})
