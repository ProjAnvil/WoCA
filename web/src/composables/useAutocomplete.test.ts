import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref, nextTick } from 'vue'
import { useAutocomplete } from './useAutocomplete'
import type { CompletionEntry } from '../types'

function entries(...names: string[]): CompletionEntry[] {
  return names.map((n) => ({ name: n, description: '' }))
}

function host(draftText: string, skills: CompletionEntry[], agents: CompletionEntry[]) {
  const enter = vi.fn()
  let api!: ReturnType<typeof useAutocomplete>
  const Host = defineComponent({
    setup() {
      const textarea = ref<any>(null)
      const draft = ref(draftText)
      const completions = ref({ skills, agents })
      api = useAutocomplete({ textarea, draft, completions, onEnter: enter })
      return () =>
        h('textarea', {
          ref: textarea,
          value: draft.value,
          onInput: (e: Event) => {
            draft.value = (e.target as HTMLTextAreaElement).value
          },
          onKeydown: (e: KeyboardEvent) => api.onKeydown(e),
        })
    },
  })
  const w = mount(Host, { attachTo: document.body })
  return { w, api, enter }
}

function key(k: string, opts: Partial<KeyboardEvent> = {}) {
  return new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true, ...opts })
}

describe('useAutocomplete', () => {
  beforeEach(() => {
    document.body.innerHTML = ''
  })

  it('"/" at line start opens the skill list, filtered by prefix', async () => {
    const { w, api } = host('/', entries('goal', 'undo', 'permissions'), [])
    const ta = w.get('textarea').element as HTMLTextAreaElement
    ta.setSelectionRange(1, 1)
    api.updateAc()
    await nextTick()
    expect(api.ac.value.open).toBe(true)
    expect(api.ac.value.kind).toBe('skill')
    expect(api.acItems.value.map((e) => e.name)).toEqual(['goal', 'undo', 'permissions'])

    // Typing narrows by prefix/includes.
    const { w: w2, api: api2 } = host('/pe', entries('goal', 'undo', 'permissions'), [])
    const ta2 = w2.get('textarea').element as HTMLTextAreaElement
    ta2.setSelectionRange(3, 3)
    api2.updateAc()
    expect(api2.acItems.value.map((e) => e.name)).toEqual(['permissions'])
  })

  it('"@" mid-text opens the agent list at the trigger index', async () => {
    const { w, api } = host('hey @rev', [], entries('reviewer', 'researcher'))
    const ta = w.get('textarea').element as HTMLTextAreaElement
    ta.setSelectionRange(7, 7)
    api.updateAc()
    await nextTick()
    expect(api.ac.value.open).toBe(true)
    expect(api.ac.value.kind).toBe('agent')
    expect(api.ac.value.start).toBe(4) // index of '@'
    expect(api.acItems.value.map((e) => e.name)).toEqual(['reviewer', 'researcher'])
  })

  it('no trigger → closed', () => {
    const { w, api } = host('plain text', entries('goal'), [])
    const ta = w.get('textarea').element as HTMLTextAreaElement
    ta.setSelectionRange(10, 10)
    api.updateAc()
    expect(api.ac.value.open).toBe(false)
  })

  it('keyboard: arrows cycle, Enter accepts and rewrites the draft, Esc closes', async () => {
    const { w, api } = host('/go', entries('goal', 'google'), [])
    const ta = w.get('textarea').element as HTMLTextAreaElement
    ta.setSelectionRange(3, 3)
    api.updateAc()
    await nextTick()

    ta.dispatchEvent(key('ArrowDown')) // goal → google
    expect(api.ac.value.index).toBe(1)
    ta.dispatchEvent(key('ArrowUp')) // back to goal
    expect(api.ac.value.index).toBe(0)

    ta.dispatchEvent(key('Enter'))
    await nextTick()
    // Draft rewritten with the trigger + name + trailing space.
    expect((ta as HTMLTextAreaElement).value).toBe('/goal ')
    expect(api.ac.value.open).toBe(false)
  })

  it('Enter delegates to submit when the popup is closed; IME composition does not', () => {
    const { w, api, enter } = host('hello', [], [])
    const ta = w.get('textarea').element as HTMLTextAreaElement
    ta.dispatchEvent(key('Enter'))
    expect(enter).toHaveBeenCalledTimes(1)

    ta.dispatchEvent(key('Enter', { isComposing: true } as Partial<KeyboardEvent>))
    expect(enter).toHaveBeenCalledTimes(1)
  })
})
