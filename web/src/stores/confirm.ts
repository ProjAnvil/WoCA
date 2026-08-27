import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface ConfirmOptions {
  title: string
  body?: string
  confirmLabel: string
  cancelLabel: string
  tone?: 'primary' | 'danger'
  /** User must type this exact string before the confirm button enables —
   *  reserved for the most destructive, unrecoverable operations. */
  requireText?: string
}

let resolver: ((ok: boolean) => void) | null = null

/**
 * Programmatic confirm dialog (replaces window.confirm everywhere): one
 * mounted WConfirmDialog renders whatever `req` currently holds; call sites
 * `await ask(...)` and branch on the boolean.
 */
export const useConfirmStore = defineStore('woca-confirm', () => {
  const req = ref<ConfirmOptions | null>(null)

  function ask(options: ConfirmOptions): Promise<boolean> {
    req.value = options
    return new Promise((resolve) => {
      resolver = resolve
    })
  }

  function settle(ok: boolean) {
    const r = resolver
    resolver = null
    req.value = null
    r?.(ok)
  }

  return { req, ask, settle }
})
