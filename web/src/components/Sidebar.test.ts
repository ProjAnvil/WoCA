import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../i18n'

const api = vi.hoisted(() => ({
  getStatus: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import Sidebar from './Sidebar.vue'
import WorkspaceSidebar from './WorkspaceSidebar.vue'
import { useUiStore } from '../stores/ui'
import { useSessionStore } from '../stores/sessions'
import { useWorkspaceStore } from '../stores/workspace'
import type { BridgeStatus } from '../api/client'

function bridgeStatus(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    edition: 'bridge',
    managed: true,
    cloud: { enrolled: true, stale: false } as never,
    ...over,
  } as BridgeStatus
}

let w: ReturnType<typeof mount> | null = null

function mountSidebar() {
  const pinia = createPinia()
  setActivePinia(pinia)
  // attachTo: focus-trap assertions need the drawer in the document —
  // happy-dom only tracks document.activeElement for attached trees.
  w = mount(Sidebar, { global: { plugins: [i18n, pinia] }, attachTo: document.body })
  return w
}

function footerLabels(): string[] {
  return (w?.findAll('.footer-settings-btn') ?? []).map((b) =>
    b.text().replace(/\s+/g, ' ').trim()
  )
}

// Mutable matchMedia stub (WorkspaceSidebar.test pattern): happy-dom's
// real matchMedia is static, but the drawer and rail shells must react to
// crossing their breakpoints live — tests flip `matches` and fire the
// registered change listener to simulate a resize. Only those two queries
// get a mutable mq (the i18n/theme layer also probes
// prefers-color-scheme through the same global and must stay inert).
const RAIL_QUERY = '(min-width: 641px) and (max-width: 1023px)'

function stubSidebarMedia(initialDrawer: boolean, initialRail = false, panel1100 = false) {
  const drawerMq = {
    matches: initialDrawer,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  };
  const railMq = {
    matches: initialRail,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  };
  const panelMq = {
    matches: panel1100,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((q: string) => {
      if (q === '(max-width: 640px)') return drawerMq
      if (q === RAIL_QUERY) return railMq
      if (q === '(max-width: 1100px)') return panelMq
      return { matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() };
    }),
  );
  return { drawerMq, railMq, panelMq };
}

function stubDrawerShell(initial: boolean) {
  return stubSidebarMedia(initial).drawerMq
}

describe('Sidebar footer', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset()
  })
  afterEach(() => {
    w?.unmount()
    w = null
  })

  it('personal serve: no status entry, no poll; settings stays', async () => {
    // /api/status does not exist on a personal deployment — the SPA
    // fallback answers with HTML and the probe never parses.
    api.getStatus.mockRejectedValue(new Error('spa fallback'))

    mountSidebar()
    await flushPromises()

    expect(footerLabels()).toEqual(['Settings'])
    expect(w?.find('.status-lamp').exists()).toBe(false)
    // Mount probe only — the 20s poll never arms without the surface.
    expect(api.getStatus).toHaveBeenCalledTimes(1)
  })

  it('bridge deployment: status entry appears with the lamp', async () => {
    api.getStatus.mockResolvedValue(bridgeStatus())

    mountSidebar()
    await flushPromises()

    expect(footerLabels()).toEqual(['Status', 'Settings'])
    expect(w?.find('.status-lamp').exists()).toBe(true)
  })
})

describe('Sidebar drawer (≤640px shells)', () => {
  let mq: ReturnType<typeof stubDrawerShell>
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset()
    // Drawer shells: the trap only engages ≤640px (happy-dom's own
    // viewport is 1024, i.e. a wide shell).
    mq = stubDrawerShell(true)
  })
  afterEach(() => {
    w?.unmount()
    w = null
    vi.unstubAllGlobals()
  })

  it('is collapsed by default: no open modifier, no backdrop', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    expect(w?.get('#app-sidebar.sidebar').classes()).not.toContain('open')
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('opens with the open modifier + backdrop; backdrop click closes', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await w?.vm.$nextTick()

    expect(w?.get('#app-sidebar.sidebar').classes()).toContain('open')
    const backdrop = w?.get('.sidebar-backdrop')
    expect(backdrop).toBeTruthy()
    await backdrop!.trigger('click')
    expect(ui.sidebarOpen).toBe(false)
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('closes on Escape while open', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()

    expect(ui.sidebarOpen).toBe(false)
    expect(w?.find('.sidebar-backdrop').exists()).toBe(false)
  })

  it('moves focus inside the drawer when it opens', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()

    const aside = document.getElementById('app-sidebar')!
    expect(aside.contains(document.activeElement)).toBe(true)
  })

  it('keeps the drawer open when Escape belongs to a hosted overlay', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()
    // The workspaces "+" opens the new-workspace dialog (a hand-rolled
    // .overlay without its own trap) and focuses its input: Esc there
    // must close the dialog, not the drawer underneath.
    await w?.findAll('.side-section-head .icon-btn')[0]!.trigger('click')
    await flushPromises()
    expect(w?.find('.overlay .ws-dialog').exists()).toBe(true)
    expect(document.activeElement?.id).toBe('ws-path-input')

    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()

    expect(w?.find('.overlay .ws-dialog').exists()).toBe(false)
    expect(ui.sidebarOpen).toBe(true)
  })

  it('disengages the trap when the window grows past 640 while open', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    const ui = useUiStore()
    ui.openSidebar()
    await flushPromises()
    const aside = document.getElementById('app-sidebar')!
    expect(aside.contains(document.activeElement)).toBe(true)
    expect(document.body.style.overflow).toBe('hidden')

    // Live resize past the breakpoint: the query flips and fires its
    // change listener. The drawer state survives (sidebarOpen stays
    // set; only its styles are media-scoped) but the trap must let
    // go — no scroll lock, no focus hold, no Tab confinement.
    mq.matches = false
    const onChange = mq.addEventListener.mock.calls.find((c) => c[0] === 'change')?.[1] as
      | ((e: MediaQueryListEvent) => void)
      | undefined
    onChange?.({ matches: false } as MediaQueryListEvent)
    await flushPromises()
    // Exactly the drawer's own listener may be registered on this mq.
    expect(mq.addEventListener).toHaveBeenCalledTimes(1)

    expect(ui.sidebarOpen).toBe(true)
    expect(document.body.style.overflow).toBe('')
    expect(aside.contains(document.activeElement)).toBe(false)

    const outside = document.createElement('button')
    document.body.append(outside)
    outside.focus()
    const event = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true })
    outside.dispatchEvent(event)
    expect(event.defaultPrevented).toBe(false)
    expect(document.activeElement).toBe(outside)
    outside.remove()
  })
})

describe('Sidebar tablet rail (641-1023px band)', () => {
  let railMq: { matches: boolean; addEventListener: ReturnType<typeof vi.fn> }

  function railButtons(): string[] {
    return (w?.findAll('.rail-btn') ?? []).map((b) => b.attributes('aria-label') ?? '')
  }

  beforeEach(() => {
    i18n.global.locale.value = 'en'
    api.getStatus.mockReset()
    // Wide desktop default: no drawer, no rail (happy-dom's own viewport).
    railMq = stubSidebarMedia(false, true).railMq
  })
  afterEach(() => {
    w?.unmount()
    w = null
    vi.unstubAllGlobals()
  })

  it('renders the rail and drops the 280px column in the band', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    expect(w?.find('#app-sidebar.sidebar').exists()).toBe(true)
    expect(w?.find('.rail-nav').exists()).toBe(true)
    // Personal serve has no status surface: sessions, new-workspace,
    // settings — the status button stays hidden with the 280px footer's
    // own condition.
    expect(railButtons()).toEqual(['Sessions', 'New workspace', 'Settings'])
    // The 280px column content moved into the flyout (Task 2); it must
    // not render inside the rail band.
    expect(w?.find('.sidebar-body').exists()).toBe(false)
    expect(w?.find('.footer-bar').exists()).toBe(false)
  })

  it('bridge deployment: the status rail button appears with its lamp', async () => {
    api.getStatus.mockResolvedValue(bridgeStatus())
    mountSidebar()
    await flushPromises()

    expect(railButtons()).toEqual(['Sessions', 'New workspace', 'Status', 'Settings'])
    expect(w?.find('.rail-btn .rail-lamp').exists()).toBe(true)
  })

  it('no rail on wide (≥1024) shells', async () => {
    railMq.matches = false
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    expect(w?.find('.rail-nav').exists()).toBe(false)
    expect(w?.find('.sidebar-body').exists()).toBe(true)
    expect(w?.find('.footer-bar').exists()).toBe(true)
    expect(w?.find('#app-sidebar.sidebar').exists()).toBe(true)
  })

  it('no rail inside the ≤640px drawer band', async () => {
    vi.unstubAllGlobals()
    railMq = stubSidebarMedia(true, false).railMq
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    expect(w?.find('.rail-nav').exists()).toBe(false)
    expect(w?.find('#app-sidebar.sidebar').exists()).toBe(true)
  })

  function sessionsBtn() {
    return w!.get('[aria-controls="rail-flyout"]')
  }

  async function openFlyout() {
    await sessionsBtn().trigger('click')
    await w!.vm.$nextTick()
  }

  it('opens the sessions flyout: expanded state, scrim, rail-open anchor', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    await openFlyout()

    const btn = sessionsBtn()
    expect(btn.attributes('aria-expanded')).toBe('true')
    expect(btn.classes()).toContain('active')
    expect(w!.find('#rail-flyout').exists()).toBe(true)
    expect(w!.find('.flyout-scrim').exists()).toBe(true)
    // The z21 anchor (audit I1): while the flyout is open the rail aside
    // carries the rail-open class — the CSS that lifts it above the scrim
    // so "click the rail button again" hits the button, not the veil.
    expect(w?.get('#app-sidebar').classes()).toContain('rail-open')
  })

  it('closes via the scrim, the rail toggle, and the close button', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    // Scrim click.
    await openFlyout()
    await w!.get('.flyout-scrim').trigger('click')
    await w!.vm.$nextTick()
    expect(w?.find('#rail-flyout').exists()).toBe(false)
    expect(w?.find('.flyout-scrim').exists()).toBe(false)
    expect(sessionsBtn().attributes('aria-expanded')).toBe('false')
    expect(w?.get('#app-sidebar').classes()).not.toContain('rail-open')

    // Toggle again from the rail button.
    await openFlyout()
    await sessionsBtn().trigger('click')
    await w!.vm.$nextTick()
    expect(w?.find('#rail-flyout').exists()).toBe(false)

    // The flyout's own close button.
    await openFlyout()
    await w!.get('.flyout-close').trigger('click')
    await w!.vm.$nextTick()
    expect(w?.find('#rail-flyout').exists()).toBe(false)
  })

  it('closes when a session is selected (drawer convention)', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    // Seed one session so the flyout's SessionList renders a clickable row.
    const store = useSessionStore()
    store.seed(
      [
        {
          id: 's1',
          title: 'Rail session',
          updatedAt: '',
          loaded: true,
          workspace: '/ws',
          workspaceName: 'ws',
        },
      ],
      { s1: [] },
    )
    await openFlyout()

    await w!.get('[data-testid="session-item"]').trigger('click')
    await w!.vm.$nextTick()
    expect(w?.find('#rail-flyout').exists()).toBe(false)
    expect(w?.find('.flyout-scrim').exists()).toBe(false)
  })

  it('traps focus while open: initial focus inside, Tab wraps, Esc closes', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    await openFlyout()
    await flushPromises()
    const flyout = document.getElementById('rail-flyout')!
    expect(flyout.contains(document.activeElement)).toBe(true)
    expect(document.body.style.overflow).toBe('hidden')

    // Tab from the last focusable stays inside the flyout.
    const focusables = Array.from(
      flyout.querySelectorAll<HTMLElement>('button, [href], input, select, textarea'),
    )
    focusables[focusables.length - 1]?.focus()
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }),
    )
    await flushPromises()
    expect(flyout.contains(document.activeElement)).toBe(true)

    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()
    expect(w?.find('#rail-flyout').exists()).toBe(false)
    expect(document.body.style.overflow).toBe('')
  })

  it('yields Esc to a hosted overlay: the dialog closes, not the flyout', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    await openFlyout()
    await flushPromises()
    // The flyout head's "+" opens the new-workspace dialog (no trap of its
    // own) and focuses its input: Esc there must close the dialog only.
    await w!.get('#rail-flyout .flyout-head-actions .icon-btn').trigger('click')
    await flushPromises()
    expect(w!.find('.overlay .ws-dialog').exists()).toBe(true)
    expect(document.activeElement?.id).toBe('ws-path-input')

    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()
    expect(w!.find('.overlay .ws-dialog').exists()).toBe(false)
    expect(w!.find('#rail-flyout').exists()).toBe(true)
    expect(sessionsBtn().attributes('aria-expanded')).toBe('true')

    // The next Esc (focus back under the flyout trap) closes the flyout.
    document.activeElement?.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
    )
    await flushPromises()
    expect(w!.find('#rail-flyout').exists()).toBe(false)
  })

  it('resets and releases when the band exits (both directions)', async () => {
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    await flushPromises()

    await openFlyout()
    await flushPromises()
    expect(document.body.style.overflow).toBe('hidden')

    // Leaving the band — growing past 1023 or shrinking under 641 both
    // flip the rail query off. The flyout must not survive the exit into
    // either neighboring band (no surprise reopen on re-entry).
    railMq.matches = false
    const onChange = railMq.addEventListener.mock.calls.find((c) => c[0] === 'change')?.[1] as
      | ((e: MediaQueryListEvent) => void)
      | undefined
    onChange?.({ matches: false } as MediaQueryListEvent)
    await flushPromises()
    // Exactly the rail's own listener may be registered on this mq.
    expect(railMq.addEventListener).toHaveBeenCalledTimes(1)

    expect(w?.find('#rail-flyout').exists()).toBe(false)
    expect(w?.find('.flyout-scrim').exists()).toBe(false)
    expect(document.body.style.overflow).toBe('')

    const outside = document.createElement('button')
    document.body.append(outside)
    outside.focus()
    const event = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true })
    outside.dispatchEvent(event)
    expect(event.defaultPrevented).toBe(false)
    expect(document.activeElement).toBe(outside)
    outside.remove()
  })

  it('stacks with the right panel overlay: Esc closes the topmost first', async () => {
    vi.unstubAllGlobals()
    railMq = stubSidebarMedia(false, true, true).railMq
    api.getStatus.mockRejectedValue(new Error('spa fallback'))
    mountSidebar()
    const panel = mount(WorkspaceSidebar, { global: { plugins: [i18n] }, attachTo: document.body })
    try {
      await flushPromises()
      // ⌘J over the band: the right panel is an overlay (≤1100) with its
      // own trap. Open the flyout first, then the panel — the panel's trap
      // is the stack top, so the first Esc closes the panel, the second
      // the flyout.
      await openFlyout()
      const ws = useWorkspaceStore()
      ws.openPanel()
      await flushPromises()
      expect(w!.find('#rail-flyout').exists()).toBe(true)

      document.activeElement?.dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
      )
      await flushPromises()
      expect(ws.panelOpen).toBe(false)
      expect(w!.find('#rail-flyout').exists()).toBe(true)

      document.activeElement?.dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }),
      )
      await flushPromises()
      expect(w!.find('#rail-flyout').exists()).toBe(false)
    } finally {
      panel.unmount()
    }
  })
})
