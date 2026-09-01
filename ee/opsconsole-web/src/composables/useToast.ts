import { reactive } from 'vue'

// The console's action-result channel: destructive and bulk actions end
// in a toast (top-right, auto-dismissing) instead of inline banners; the
// list pages keep their banner only for load failures.
export interface OpsToast {
  id: number
  message: string
  kind: 'info' | 'success' | 'error'
}

const toasts = reactive<OpsToast[]>([])
let seq = 0

export function useToasts(): OpsToast[] {
  return toasts
}

export function pushToast(message: string, kind: OpsToast['kind'] = 'info', ttlMs = 3500): number {
  const id = ++seq
  toasts.push({ id, message, kind })
  setTimeout(() => dismissToast(id), ttlMs)
  return id
}

export function dismissToast(id: number): void {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
}
