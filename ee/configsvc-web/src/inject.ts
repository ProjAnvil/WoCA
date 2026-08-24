import { inject } from 'vue'
import type { Settings } from './types'

// The App provides one shared context: the loaded settings snapshot, the
// draft fields the operator touched, and the save/api plumbing. The
// context is provided through reactive(), which unwraps the refs — the
// interface below describes that unwrapped view (a plain object of refs
// would leave every template binding reading the Ref wrapper itself, and
// the console would render empty fields despite loaded data).
export interface CfgContext {
  s: Settings | null
  d: (field: string) => string
  /** Effective value of a plain string field: the operator's draft when
   *  touched, else the loaded setting. Conditional rendering and
   *  cross-field bindings must use this, not the loaded snapshot — the
   *  snapshot does not react to drafts. */
  v: (field: string) => string
  setDraft: (field: string, v: string) => void
  save: (extra?: Record<string, unknown>) => Promise<void>
  saving: boolean
  savedFlash: string
  error: string
  api: (path: string, init?: RequestInit) => Promise<Response>
}

export function injectCfg(): CfgContext {
  const ctx = inject<CfgContext>('cfg')
  if (!ctx) throw new Error('cfg context not provided')
  return ctx
}
