import { ref, type Ref } from 'vue'
import type { ChatItem, ContextInfo } from '../../types'
import { toErrorMessage } from '../../lib/errors'
import * as api from '../../api/client'

export interface ContextDeps {
  activeId: Ref<string>
  items: Ref<Record<string, ChatItem[]>>
  demo: Ref<boolean>
}

/*
 * Context-usage snapshot for the chat input's context indicator. Hover
 * refreshes it on demand; a run finishing refreshes it in the background so
 * the always-visible chip number stays current. An empty session (nothing
 * sent yet) has no usage to report — the chip stays disabled until the
 * first message lands. Local-only sessions (demo mode / failed create)
 * have no server-side transcript either, so they keep the last snapshot.
 */
export function createContextTracker(deps: ContextDeps) {
  const { activeId, items, demo } = deps

  const contextInfo = ref<ContextInfo | null>(null)
  const contextLoading = ref(false)
  const contextError = ref('')

  async function refreshContext() {
    const sid = activeId.value
    if (!sid || sid.startsWith('tmp-') || demo.value) return
    if ((items.value[sid] ?? []).length === 0) {
      contextInfo.value = null
      return
    }
    contextLoading.value = true
    contextError.value = ''
    try {
      contextInfo.value = await api.getSessionContext(sid)
    } catch (e) {
      contextError.value = toErrorMessage(e)
    } finally {
      contextLoading.value = false
    }
  }

  return { contextInfo, contextLoading, contextError, refreshContext }
}

export type ContextTracker = ReturnType<typeof createContextTracker>
