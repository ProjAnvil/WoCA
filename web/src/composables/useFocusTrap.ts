import { nextTick, onScopeDispose, watch, type Ref } from 'vue'

const FOCUSABLE = [
  'a[href]:not([disabled])',
  'button:not([disabled])',
  'textarea:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

// Reference-counted body scroll lock: stacked overlays (confirm over a
// modal) must not unlock the page while an outer layer is still open.
let scrollLockCount = 0

// Stack of active traps; only the topmost acts on Tab/Escape so layered
// dialogs behave like native sheets (inner consumes first).
const trapStack: Array<(e: KeyboardEvent) => void> = []

/** Focuses the first focusable descendant of `root` (or root itself). */
export function focusFirst(root: HTMLElement): void {
  const first = root.querySelector<HTMLElement>(FOCUSABLE)
  ;(first ?? root).focus({ preventScroll: true })
}

/**
 * Traps Tab focus inside `container` while `active` is true, closes on Esc
 * via `opts.esc`, locks body scroll, and restores focus to the trigger
 * element on deactivate.
 */
export function useFocusTrap(
  container: Ref<HTMLElement | null>,
  active: Ref<boolean>,
  opts: { esc?: () => void } = {},
) {
  let restoreTo: HTMLElement | null = null
  let engaged = false

  function onKeyDown(e: KeyboardEvent) {
    if (trapStack[trapStack.length - 1] !== onKeyDown) return
    if (e.key === 'Escape') {
      e.stopPropagation()
      opts.esc?.()
      return
    }
    if (e.key !== 'Tab') return
    const root = container.value
    if (!root) return
    const focusables = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE))
    if (!focusables.length) return
    const first = focusables[0]
    const last = focusables[focusables.length - 1]
    const current = document.activeElement
    if (e.shiftKey) {
      if (current === first || !root.contains(current)) {
        e.preventDefault()
        last.focus()
      }
    } else if (current === last || !root.contains(current)) {
      e.preventDefault()
      first.focus()
    }
  }

  function disengage() {
    if (!engaged) return
    engaged = false
    const i = trapStack.indexOf(onKeyDown)
    if (i >= 0) trapStack.splice(i, 1)
    document.removeEventListener('keydown', onKeyDown)
    scrollLockCount = Math.max(0, scrollLockCount - 1)
    if (scrollLockCount === 0) document.body.style.overflow = ''
    restoreTo?.focus({ preventScroll: true })
    restoreTo = null
  }

  watch(
    active,
    async (on) => {
      if (on && !engaged) {
        engaged = true
        restoreTo = document.activeElement instanceof HTMLElement ? document.activeElement : null
        scrollLockCount++
        document.body.style.overflow = 'hidden'
        document.addEventListener('keydown', onKeyDown)
        trapStack.push(onKeyDown)
        await nextTick()
        const root = container.value
        if (root) {
          const first = root.querySelector<HTMLElement>(FOCUSABLE)
          ;(first ?? root).focus({ preventScroll: true })
        }
      } else if (!on) {
        disengage()
      }
    },
    { immediate: true },
  )

  onScopeDispose(disengage)
}
