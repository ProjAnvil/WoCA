import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../i18n'

import WorkspaceSidebar from './WorkspaceSidebar.vue'
import { useWorkspaceStore } from '../stores/workspace'

// ≤1100px the workspace panel turns from a static split pane into an
// overlay drawer (panels.css @media 1100): only drawer shells get the
// focus trap + Esc. matchMedia is stubbed per shell (ChatPanel.flip
// pattern) because happy-dom evaluates queries against its own viewport.
function stubShell(drawer: boolean) {
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockReturnValue({
      matches: drawer,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }),
  )
}

let w: ReturnType<typeof mount> | null = null

function mountPanel() {
  const pinia = createPinia()
  setActivePinia(pinia)
  w = mount(WorkspaceSidebar, {
    global: {
      plugins: [i18n, pinia],
      // The tree/preview/changes panes are orthogonal to the trap; stub
      // them so focus targets are exactly the header + tab buttons.
      stubs: { WorkspaceTree: true, FilePreview: true, ChangesTab: true },
    },
    attachTo: document.body,
  })
  return w
}

function esc() {
  document.activeElement?.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
  )
}

describe('WorkspaceSidebar drawer behavior', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })
  afterEach(() => {
    w?.unmount()
    w = null
    vi.unstubAllGlobals()
  })

  it('wide shell: open panel takes no focus, Esc does not close', async () => {
    stubShell(false)
    mountPanel()
    const store = useWorkspaceStore()
    store.openPanel()
    await flushPromises()

    const aside = document.querySelector('.right-sidebar') as HTMLElement
    expect(aside.contains(document.activeElement)).toBe(false)
    esc()
    expect(store.panelOpen).toBe(true)
  })

  it('drawer shell: open moves focus inside; Tab wraps; Esc closes and restores the trigger', async () => {
    stubShell(true)
    const trigger = document.createElement('button')
    trigger.id = 'ws-trigger'
    document.body.append(trigger)
    trigger.focus()
    mountPanel()
    const store = useWorkspaceStore()
    store.openPanel()
    await flushPromises()

    const aside = document.querySelector('.right-sidebar') as HTMLElement
    expect(aside.contains(document.activeElement)).toBe(true)

    const buttons = Array.from(aside.querySelectorAll('button'))
    expect(buttons.length).toBeGreaterThan(1)
    buttons[buttons.length - 1]!.focus()
    buttons[buttons.length - 1]!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }),
    )
    expect(document.activeElement).toBe(buttons[0])

    esc()
    expect(store.panelOpen).toBe(false)
    await flushPromises()
    // Disengage returns focus to the invoker (the ⌘J/toolbar trigger).
    expect(document.activeElement).toBe(trigger)
    trigger.remove()
  })
})
