import { defineStore } from 'pinia'
import { ref } from 'vue'

// Toast notifications: the in-app replacement for window.alert — the
// transient-error surface everywhere else in the app is an inline hint,
// and these call sites (session delete, mode/thinking switches) have no
// natural inline slot in the tree. One Pinia store so any component can
// raise a toast and one renderer (App root) owns the view.
export interface ToastAction {
  label: string
  run: () => void
}

export interface Toast {
  id: number
  message: string
  kind: 'error' | 'warn' | 'success' | 'info'
  action?: ToastAction
}

const MAX_STACKED = 3

let nextID = 1

export const useToastStore = defineStore('toast', () => {
  const toasts = ref<Toast[]>([])
  const timers = new Map<number, ReturnType<typeof setTimeout>>()

  function push(message: string, kind: Toast['kind'] = 'info', ttl = 5000, action?: ToastAction) {
    // At most MAX_STACKED toasts on screen: the oldest leaves first.
    while (toasts.value.length >= MAX_STACKED) {
      dismiss(toasts.value[0].id)
    }
    const id = nextID++
    toasts.value.push({ id, message, kind, action })
    timers.set(
      id,
      setTimeout(() => dismiss(id), ttl),
    )
  }

  function dismiss(id: number) {
    const timer = timers.get(id)
    if (timer) {
      clearTimeout(timer)
      timers.delete(id)
    }
    toasts.value = toasts.value.filter((t) => t.id !== id)
  }

  function error(e: unknown, action?: ToastAction) {
    push(e instanceof Error ? e.message : String(e), 'error', 5000, action)
  }

  function warn(message: string, action?: ToastAction) {
    push(message, 'warn', 5000, action)
  }

  function success(message: string, action?: ToastAction) {
    push(message, 'success', 4000, action)
  }

  return { toasts, push, dismiss, error, warn, success }
})
