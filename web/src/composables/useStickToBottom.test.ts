import { describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import { useStickToBottom } from './useStickToBottom'

function makeScrollable(scrollHeight: number, clientHeight: number) {
  const el = document.createElement('div')
  Object.defineProperty(el, 'scrollHeight', { value: scrollHeight, configurable: true })
  Object.defineProperty(el, 'clientHeight', { value: clientHeight, configurable: true })
  return el
}

describe('useStickToBottom', () => {
  it('follows to the bottom while pinned', async () => {
    const el = makeScrollable(1000, 300)
    const raf = vi.fn((cb: FrameRequestCallback) => {
      cb(0)
      return 0
    })
    vi.stubGlobal('requestAnimationFrame', raf)
    const { pinned, follow } = useStickToBottom(ref(el))
    expect(pinned.value).toBe(true)
    follow()
    expect(el.scrollTop).toBe(1000)
    vi.unstubAllGlobals()
  })

  it('detaches on user scroll and counts missed updates', () => {
    const el = makeScrollable(1000, 300)
    el.scrollTop = 0 // far from the bottom
    const { pinned, missed, follow, jumpToLatest } = useStickToBottom(ref(el))
    el.dispatchEvent(new Event('scroll'))
    expect(pinned.value).toBe(false)

    follow()
    follow()
    expect(missed.value).toBe(2)
    expect(el.scrollTop).toBe(0) // no yank while reading

    jumpToLatest()
    expect(pinned.value).toBe(true)
    expect(missed.value).toBe(0)
  })

  it('re-pins when scrolled back near the bottom', () => {
    const el = makeScrollable(1000, 300)
    const { pinned } = useStickToBottom(ref(el))
    el.scrollTop = 0
    el.dispatchEvent(new Event('scroll'))
    expect(pinned.value).toBe(false)
    // 120px threshold: within it counts as pinned again.
    el.scrollTop = 1000 - 300 - 100
    el.dispatchEvent(new Event('scroll'))
    expect(pinned.value).toBe(true)
  })
})
