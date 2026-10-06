import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatItemView from './ChatItemView.vue'
import { useSettingsStore } from '../stores/settings'
import { useServerSettingsStore } from '../stores/serverSettings'
import type { ChatItem } from '../types'

function mountItem(item: ChatItem) {
  return mount(ChatItemView, {
    props: { item },
    global: { plugins: [i18n, createPinia()] },
  })
}

const user = (fresh?: boolean): ChatItem =>
  ({ id: 'u1', kind: 'user', content: 'hi', ...(fresh ? { fresh } : {}) }) as ChatItem

describe('user message entrance marking', () => {
  it('marks fresh live-pushed user bubbles', () => {
    const w = mountItem(user(true))
    expect(w.get('.message').classes()).toContain('message--new')
  })
  it('does not mark history-loaded user bubbles', () => {
    const w = mountItem(user())
    expect(w.get('.message').classes()).not.toContain('message--new')
  })
})

const assistant = (content = '**hi** _world_'): ChatItem =>
  ({ id: 'a1', kind: 'assistant', content, streaming: false }) as ChatItem

function mountAssistant(content?: string) {
  return mountItem(assistant(content))
}

function stubClipboard() {
  const write = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
  return write
}

describe('bubble copy split button', () => {
  it('main button copies the markdown source', async () => {
    const write = stubClipboard()
    const w = mountAssistant()
    await w.get('.bubble-action.copy-main').trigger('click')
    expect(write).toHaveBeenCalledWith('**hi** _world_')
  })

  it('dropdown plain-text item copies stripped text', async () => {
    const write = stubClipboard()
    const w = mountAssistant()
    try {
      await w.get('.bubble-action.copy-side').trigger('click') // span 上 toggle 打开
      // 菜单 Teleport 到 body——从 document 查（wrapper 查不到）
      const items = Array.from(document.body.querySelectorAll('[role="menuitem"]'))
      expect(items).toHaveLength(2)
      items[1].dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await w.vm.$nextTick()
      expect(write).toHaveBeenCalledWith('hi world')
    } finally {
      w.unmount()
    }
  })

  it('pins the action bar while the menu is open', async () => {
    const w = mountAssistant()
    expect(w.get('.bubble-actions').classes()).not.toContain('is-menu-open')
    await w.get('.bubble-action.copy-side').trigger('click')
    expect(w.get('.bubble-actions').classes()).toContain('is-menu-open')
  })

  it('hides the action bar while still streaming', () => {
    const w = mountItem({ id: 'a1', kind: 'assistant', content: 'partial', streaming: true } as ChatItem)
    expect(w.find('.bubble-actions').exists()).toBe(false)
  })
})

describe('reasoning card meta', () => {
  const reasoning = (over: Record<string, unknown>): ChatItem =>
    ({ id: 'r1', kind: 'reasoning', content: 'think', streaming: false, ...over }) as ChatItem

  it('ticks live seconds while the thinking segment streams', () => {
    const w = mountItem(reasoning({ streaming: true, startedAt: Date.now() }))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.thinkingSeconds', { count: 0 }))
    w.unmount()
  })

  it('shows the total once known', () => {
    const w = mountItem(reasoning({ durationMs: 3000 }))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.thoughtFor', { count: 3 }))
  })

  it('falls back to the char count for legacy blocks without durations', () => {
    const w = mountItem(reasoning({}))
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.charCount', { count: 5 }))
  })
})

describe('text bubble variants', () => {
  it('renders image attachments on user bubbles', () => {
    const w = mountItem({
      id: 'u1',
      kind: 'user',
      content: 'look',
      attachments: [
        { id: 'a', name: 'shot.png', mimeType: 'image/png', dataUrl: 'data:image/png;base64,x', base64: 'x' },
      ],
    } as ChatItem)
    const img = w.get('.msg-attachment-img')
    expect(img.attributes('alt')).toBe('shot.png')
    expect(img.attributes('src')).toBe('data:image/png;base64,x')
  })

  it('shows only the cursor for an empty streaming bubble', () => {
    const w = mountItem({ id: 'a1', kind: 'assistant', content: '', streaming: true } as ChatItem)
    expect(w.find('.markdown-body').exists()).toBe(false)
    expect(w.find('.cursor').exists()).toBe(true)
  })
})

describe('debug copy action', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  function stubClipboard() {
    const write = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
    return write
  }

  it('copies the log-retrieval context when the observability log is on', async () => {
    const write = stubClipboard()
    const w = mountItem({
      id: 'a1',
      kind: 'assistant',
      content: 'hi',
      streaming: false,
      ts: 1_760_000_000_000,
      srcRun: 'run-9',
    } as ChatItem)
    const server = useServerSettingsStore()
    server.debugLog = true
    await w.vm.$nextTick()

    await w.get('.bubble-action.debug').trigger('click')
    const copied = write.mock.calls[0][0] as string
    expect(copied).toContain('## woca debug context')
    expect(copied).toContain('- run: run-9')
    // logDir is a read-only computed from the server snapshot; with no
    // server configured it falls back to the default log location.
    expect(copied).toContain('~/.woca/logs/woca.log')
  })

  it('hides the debug button when the observability log is off', async () => {
    mountItem({ id: 'a1', kind: 'assistant', content: 'hi', streaming: false } as ChatItem)
    const server = useServerSettingsStore()
    server.debugLog = false
    const w = mountItem({ id: 'a2', kind: 'assistant', content: 'hi', streaming: false } as ChatItem)
    expect(w.find('.bubble-action.debug').exists()).toBe(false)
  })
})

describe('collapsible cards', () => {
  const openCard = async (w: ReturnType<typeof mountItem>) => {
    await w.get('.w-collapse-head').trigger('click')
  }

  it('tool call: running hint without a result, input+output once merged', async () => {
    // Cards start collapsed (autoCollapseTools default); open to inspect.
    const call = {
      id: 't1',
      kind: 'tool_call',
      toolCallId: 'tc1',
      name: 'Bash',
      args: { cmd: 'ls' },
    } as ChatItem
    const w = mountItem(call)
    expect(w.get('.stream-card').text()).toContain('Bash')
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.running'))
    expect(w.find('.tool-args').exists()).toBe(false)
    await openCard(w)
    expect(w.get('.tool-args').text()).toContain('"cmd"')
    expect(w.get('.stream-card').text()).not.toContain(i18n.global.t('chat.output'))

    const w2 = mountItem({ ...call, result: 'ok-output' } as ChatItem)
    await openCard(w2)
    expect(w2.get('.tool-output').text()).toContain('ok-output')
  })

  it('orphan tool result renders a standalone card', async () => {
    const w = mountItem({ id: 'r9', kind: 'tool_result', toolCallId: 'tcX', content: 'orphan' } as ChatItem)
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.toolOutput'))
    await openCard(w)
    expect(w.get('.tool-output').text()).toContain('orphan')
  })

  it('reasoning: live block stays open and collapses when done', async () => {
    const w = mountItem({ id: 'r1', kind: 'reasoning', content: 'deep', streaming: true } as ChatItem)
    expect(w.get('.reasoning-body').text()).toContain('deep')
    expect(w.find('.reasoning-cursor').exists()).toBe(true)

    await w.setProps({
      item: { id: 'r1', kind: 'reasoning', content: 'deep', streaming: false } as ChatItem,
    })
    expect(w.find('[inert]').exists()).toBe(true)
  })

  it('goal note: open while looping, collapses on the unified reply', async () => {
    const w = mountItem({ id: 'g1', kind: 'goal_note', content: 'cycle 1', streaming: true } as ChatItem)
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.goalLoopLive'))
    await w.setProps({
      item: { id: 'g1', kind: 'goal_note', content: 'cycle 1', streaming: false } as ChatItem,
    })
    expect(w.get('.stream-card').text()).toContain(i18n.global.t('chat.goalLoopDone'))
    expect(w.find('[inert]').exists()).toBe(true)
  })
})
