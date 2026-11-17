import { afterEach, describe, expect, it, vi } from 'vitest'
import { effectScope, ref, type Ref } from 'vue'
import { flushPromises } from '@vue/test-utils'
import { useFocusTrap } from './useFocusTrap'

// The composable registers document listeners, body scroll locks and a
// module-level trapStack entry — every test owns an effectScope so a
// failure mid-test cannot leak an engaged trap into the next one
// (onScopeDispose disengages on scope.stop, mirroring unmount).
const scopes: Array<ReturnType<typeof effectScope>> = []

/** Mounts a trap inside a fresh scope, as a component would. */
function mountTrap(container: Ref<HTMLElement | null>, active: Ref<boolean>, esc?: () => void) {
  const scope = effectScope()
  scopes.push(scope)
  scope.run(() => useFocusTrap(container, active, esc ? { esc } : {}))
  return scope
}

/** A container with two focusable children, attached so focus applies. */
function makeContainer(label: string): HTMLElement {
  const root = document.createElement('div')
  root.dataset.trap = label
  for (const id of [`${label}-first`, `${label}-last`]) {
    const btn = document.createElement('button')
    btn.id = id
    btn.type = 'button'
    root.append(btn)
  }
  document.body.append(root)
  return root
}

function el(id: string): HTMLElement {
  const found = document.getElementById(id)
  if (!found) throw new Error(`missing #${id}`)
  return found
}

function key(keyboard: { key: string; shiftKey?: boolean }) {
  const event = new KeyboardEvent('keydown', {
    key: keyboard.key,
    shiftKey: keyboard.shiftKey ?? false,
    bubbles: true,
    cancelable: true,
  })
  document.activeElement?.dispatchEvent(event)
  return event
}

afterEach(() => {
  while (scopes.length) scopes.pop()!.stop()
  document.body.innerHTML = ''
})

describe('useFocusTrap', () => {
  it('wraps Tab from the last focusable back to the first', async () => {
    const root = makeContainer('wrap')
    const active = ref(false)
    mountTrap(ref(root), active)
    active.value = true
    await flushPromises()
    // Engage focuses the first focusable descendant.
    expect(document.activeElement).toBe(el('wrap-first'))

    el('wrap-last').focus()
    const event = key({ key: 'Tab' })
    expect(event.defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(el('wrap-first'))
  })

  it('pulls focus back inside when Tab is pressed outside the container', async () => {
    const root = makeContainer('pull')
    const outside = document.createElement('button')
    outside.id = 'pull-outside'
    document.body.append(outside)
    const active = ref(false)
    mountTrap(ref(root), active)
    active.value = true
    await flushPromises()

    outside.focus()
    key({ key: 'Tab' })
    expect(document.activeElement).toBe(el('pull-first'))
  })

  it('wraps Shift+Tab from the first focusable back to the last', async () => {
    const root = makeContainer('reverse')
    const active = ref(false)
    mountTrap(ref(root), active)
    active.value = true
    await flushPromises()
    expect(document.activeElement).toBe(el('reverse-first'))

    const event = key({ key: 'Tab', shiftKey: true })
    expect(event.defaultPrevented).toBe(true)
    expect(document.activeElement).toBe(el('reverse-last'))
  })

  it('attaches no listeners and takes no effect while inactive', () => {
    const root = makeContainer('idle')
    const active = ref(false)
    const esc = vi.fn()
    const addSpy = vi.spyOn(document, 'addEventListener')
    mountTrap(ref(root), active, esc)

    expect(addSpy).not.toHaveBeenCalledWith('keydown', expect.any(Function))

    const outside = document.createElement('button')
    outside.id = 'idle-outside'
    document.body.append(outside)
    outside.focus()
    const event = key({ key: 'Tab' })
    expect(event.defaultPrevented).toBe(false)
    expect(document.activeElement).toBe(outside)
    key({ key: 'Escape' })
    expect(esc).not.toHaveBeenCalled()
    expect(document.body.style.overflow).toBe('')
  })

  it('routes Escape to opts.esc only while active', async () => {
    const root = makeContainer('esc')
    const active = ref(false)
    const esc = vi.fn()
    mountTrap(ref(root), active, esc)

    key({ key: 'Escape' })
    expect(esc).not.toHaveBeenCalled()

    active.value = true
    await flushPromises()
    key({ key: 'Escape' })
    expect(esc).toHaveBeenCalledTimes(1)

    active.value = false
    await flushPromises()
    key({ key: 'Escape' })
    expect(esc).toHaveBeenCalledTimes(1)
  })

  it('returns focus to the previously-focused element on close', async () => {
    const root = makeContainer('restore')
    const trigger = document.createElement('button')
    trigger.id = 'restore-trigger'
    document.body.append(trigger)
    trigger.focus()

    const active = ref(false)
    mountTrap(ref(root), active)
    active.value = true
    await flushPromises()
    expect(document.activeElement).toBe(el('restore-first'))

    active.value = false
    await flushPromises()
    expect(document.activeElement).toBe(trigger)
  })

  // Nested walk-through (HitlStack trap + GrantsPanel trap shape): with
  // two engaged traps only the topmost acts; closing it hands Esc and Tab
  // back to the outer trap, and the scroll lock outlives it (refcount).
  it('lets only the topmost trap act while two are stacked', async () => {
    const outer = makeContainer('outer')
    const inner = makeContainer('inner')
    const outerActive = ref(false)
    const innerActive = ref(false)
    const outerEsc = vi.fn()
    const innerEsc = vi.fn()
    mountTrap(ref(outer), outerActive, outerEsc)
    mountTrap(ref(inner), innerActive, innerEsc)

    outerActive.value = true
    await flushPromises()
    expect(document.activeElement).toBe(el('outer-first'))
    expect(document.body.style.overflow).toBe('hidden')

    innerActive.value = true
    await flushPromises()
    expect(document.activeElement).toBe(el('inner-first'))

    // Inner trap owns Tab: wrapping stays inside the inner container even
    // though the outer handler is on the same document.
    el('inner-last').focus()
    key({ key: 'Tab' })
    expect(document.activeElement).toBe(el('inner-first'))

    // Inner trap owns Esc: the outer trap's esc must not fire.
    key({ key: 'Escape' })
    expect(innerEsc).toHaveBeenCalledTimes(1)
    expect(outerEsc).not.toHaveBeenCalled()

    // Closing the inner trap falls focus back to the outer layer and
    // keeps the page locked while the outer overlay is still open.
    innerActive.value = false
    await flushPromises()
    expect(document.activeElement).toBe(el('outer-first'))
    expect(document.body.style.overflow).toBe('hidden')

    // Esc and Tab now belong to the outer trap again.
    key({ key: 'Escape' })
    expect(outerEsc).toHaveBeenCalledTimes(1)
    expect(innerEsc).toHaveBeenCalledTimes(1)
    el('outer-last').focus()
    key({ key: 'Tab' })
    expect(document.activeElement).toBe(el('outer-first'))

    // Last trap out releases the scroll lock.
    outerActive.value = false
    await flushPromises()
    expect(document.body.style.overflow).toBe('')
  })
})
