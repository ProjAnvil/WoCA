// The command registry: the copy-last action must address the MOST RECENT
// assistant item (D2's reverse scan) and disappear when none exists.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { defineComponent } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import { useAppCommands, type PaletteCommand } from './useAppCommands'
import { useSessionStore } from '../stores/sessions'
import type { ChatItem } from '../types'

// useAppCommands calls useI18n(), which needs a component setup context —
// run it inside a host component mounted with the app i18n + a fresh pinia.
function mountCommands(items: ChatItem[]) {
  const pinia = createPinia()
  setActivePinia(pinia)
  let cmds: ReturnType<typeof useAppCommands> | undefined
  const app = mount(
    defineComponent({
      setup() {
        cmds = useAppCommands()
        return () => null
      },
    }),
    { global: { plugins: [i18n, pinia] } },
  )
  const store = useSessionStore()
  store.seed(
    [{ id: 's1', title: 'S', updatedAt: '', loaded: true, workspace: '', workspaceName: '' }],
    { s1: items },
  )
  return { app, cmds: cmds! }
}

const assistant = (content: string): ChatItem => ({ id: content, kind: 'assistant', content, streaming: false }) as ChatItem

function stubClipboard() {
  const write = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
  return write
}

describe('copy-last command', () => {
  it('addresses the most recent assistant reply', async () => {
    const write = stubClipboard()
    const { app, cmds } = mountCommands([
      { id: 'u1', kind: 'user', content: 'q1' } as ChatItem,
      assistant('first answer'),
      { id: 'u2', kind: 'user', content: 'q2' } as ChatItem,
      assistant('last answer'),
    ])
    await app.vm.$nextTick()
    const copy = cmds.value.find((c: PaletteCommand) => c.id === 'action:copyLast')
    expect(copy).toBeTruthy()
    copy!.run()
    expect(write).toHaveBeenCalledWith('last answer')
    app.unmount()
  })

  it('is absent when the transcript has no assistant reply', async () => {
    const { app, cmds } = mountCommands([{ id: 'u1', kind: 'user', content: 'q' } as ChatItem])
    await app.vm.$nextTick()
    expect(cmds.value.find((c: PaletteCommand) => c.id === 'action:copyLast')).toBeUndefined()
    app.unmount()
  })
})
