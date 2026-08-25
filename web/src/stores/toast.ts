import { defineStore } from 'pinia'
import { ref } from 'vue'

// Toast notifications: the in-app replacement for window.alert — the
// transient-error surface everywhere else in the app is an inline hint,
// and these three call sites (session delete, mode/thinking switches)
// have no natural inline slot in the tree. One Pinia store so any
// component can raise a toast and one renderer (App root) owns the view.
export interface Toast {
  id: number
  message: string
  kind: 'error' | 'info'
}

let nextID = 1

export const useToastStore = defineStore('toast', () => {
  const toasts = ref<Toast[]>([])
  const timers = new Map<number, ReturnType<typeof setTimeout>>()

  function push(message: string, kind: Toast['kind'] = 'error', ttl = 5000) {
    const id = nextID++
    toasts.value.push({ id, message, kind })
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

  function error(e: unknown) {
    push(e instanceof Error ? e.message : String(e), 'error')
  }

  return { toasts, push, dismiss, error }
})
