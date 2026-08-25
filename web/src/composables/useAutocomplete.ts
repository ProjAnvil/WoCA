import { computed, ref, type Ref } from 'vue'
import type { CompletionEntry } from '../types'

// The composer's "/"-skill and "@"-subagent autocomplete. ArrowUp/Down move
// the highlight, Enter/Tab accept, Esc dismisses; Enter outside the popup is
// delegated to onEnter (submit). Extracted from ChatPanel so the trigger
// grammar and keyboard behavior are testable alone.
export type AcKind = 'skill' | 'agent'

export interface AutocompleteOptions {
  /** The composer's textarea (caret position + focus restore). */
  textarea: Ref<HTMLTextAreaElement | null>
  /** The composer's draft (v-model string). */
  draft: Ref<string>
  /** The "/" and "@" inventories (workspace-scoped, from /api/completions). */
  completions: Ref<{ skills: CompletionEntry[]; agents: CompletionEntry[] }>
  /** Enter outside the popup (submit). */
  onEnter: () => void
}

export function useAutocomplete(opts: AutocompleteOptions) {
  const { textarea, draft, completions, onEnter } = opts
  const ac = ref<{ open: boolean; kind: AcKind; query: string; start: number; index: number }>({
    open: false,
    kind: 'skill',
    query: '',
    start: 0,
    index: 0,
  })

  const acItems = computed<CompletionEntry[]>(() => {
    if (!ac.value.open) return []
    const q = ac.value.query.toLowerCase()
    const all = ac.value.kind === 'skill' ? completions.value.skills : completions.value.agents
    const scored = all.filter((e) => {
      const n = e.name.toLowerCase()
      return n.startsWith(q) || n.includes(q)
    })
    return scored.slice(0, 12)
  })

  function updateAc() {
    const el = textarea.value
    if (!el) return
    const before = draft.value.slice(0, el.selectionStart ?? draft.value.length)

    const skill = before.match(/^\/([a-zA-Z0-9_-]*)$/)
    if (skill) {
      setAc('skill', skill[1], 0)
      return
    }
    const agent = before.match(/(^|\s)@([a-zA-Z0-9_-]*)$/)
    if (agent) {
      // index of the '@' trigger inside the draft
      setAc('agent', agent[2], before.length - agent[2].length - 1)
      return
    }
    ac.value.open = false
  }

  // start is the draft index of the trigger character ('/' or '@'); the
  // query to filter by begins at start+1.
  function setAc(kind: AcKind, query: string, start: number) {
    const changed = query !== ac.value.query || kind !== ac.value.kind || !ac.value.open
    ac.value = { open: true, kind, query, start, index: changed ? 0 : ac.value.index }
  }

  function closeAc() {
    ac.value.open = false
  }

  // Delay so clicking a popup item fires before the popup unmounts.
  function onBlurCloseAc() {
    window.setTimeout(closeAc, 150)
  }

  function acceptAc(entry?: CompletionEntry) {
    const target = entry ?? acItems.value[ac.value.index]
    if (!target || !textarea.value) return
    const el = textarea.value
    const caret = el.selectionStart ?? draft.value.length
    const prefix = ac.value.kind === 'skill' ? '/' : '@'
    draft.value =
      draft.value.slice(0, ac.value.start) + prefix + target.name + ' ' + draft.value.slice(caret)
    closeAc()
    el.focus()
  }

  function onKeydown(e: KeyboardEvent) {
    if (ac.value.open && acItems.value.length) {
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        ac.value.index = (ac.value.index + 1) % acItems.value.length
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        ac.value.index = (ac.value.index - 1 + acItems.value.length) % acItems.value.length
        return
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault()
        acceptAc()
        return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        closeAc()
        return
      }
    }
    // Enter submits — except while an IME composition is active. WebKit
    // keeps the composing text out of v-model until compositionend, so an
    // unguarded Enter-to-commit in the Wails WebView sends an empty draft
    // (nothing happens, no bubble) instead of confirming the candidate.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault()
      onEnter()
    }
  }

  return { ac, acItems, updateAc, closeAc, onBlurCloseAc, acceptAc, onKeydown }
}
