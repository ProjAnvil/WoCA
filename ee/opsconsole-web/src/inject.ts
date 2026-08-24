import { inject } from 'vue'

// The App provides one shared context: the console-session plumbing. The
// session cookie carries the console identity; no credential material
// rides the API calls themselves. Provided through reactive() (unwrapped
// refs — see configsvc-web's inject.ts for the binding rationale).
export interface OpsContext {
  api: (path: string, init?: RequestInit) => Promise<Response>
}

export function injectOps(): OpsContext {
  const ctx = inject<OpsContext>('ops')
  if (!ctx) throw new Error('ops context not provided')
  return ctx
}
