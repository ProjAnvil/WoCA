import { nextTick, ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { useSmoothStream } from './useSmoothStream'

function setup() {
  const content = ref('')
  const streaming = ref(false)
  const ticks: number[] = []
  let frames: FrameRequestCallback[] = []
  const raf = vi.fn((cb: FrameRequestCallback) => {
    frames.push(cb)
    return frames.length
  })
  vi.stubGlobal('requestAnimationFrame', raf)
  const s = useSmoothStream({
    content: () => content.value,
    streaming: () => streaming.value,
    onTick: () => ticks.push(1),
  })
  const drain = (n: number, t = 1_000_000) => {
    for (let i = 0; i < n; i++) {
      const f = frames
      frames = []
      t += 40
      f.forEach((cb) => cb(t))
    }
  }
  const settle = async (mutate: () => void) => {
    mutate()
    await nextTick() // watchers flush on the microtask queue
  }
  return { s, content, streaming, ticks, drain, settle }
}

describe('useSmoothStream', () => {
  it('passes content through when not streaming', async () => {
    const { s, content, settle } = setup()
    await settle(() => {
      content.value = 'hello'
    })
    expect(s.display.value).toBe('hello')
  })

  it('releases words gradually while streaming', async () => {
    const { s, content, streaming, drain, settle } = setup()
    await settle(() => {
      streaming.value = true
    })
    await settle(() => {
      content.value = 'aa bb cc'
    })
    drain(1)
    expect(s.display.value.length).toBeLessThan('aa bb cc'.length)
    drain(20)
    expect(s.display.value).toBe('aa bb cc')
  })

  it('flushes to the exact store content when streaming ends', async () => {
    const { s, content, streaming, drain, settle } = setup()
    await settle(() => {
      streaming.value = true
    })
    await settle(() => {
      content.value = 'xx yy'
    })
    drain(1)
    await settle(() => {
      streaming.value = false
    })
    expect(s.display.value).toBe('xx yy')
  })

  it('accelerates on big backlog (code-block burst)', async () => {
    const { s, content, streaming, drain, settle } = setup()
    await settle(() => {
      streaming.value = true
    })
    await settle(() => {
      content.value = 'a'.repeat(400)
    })
    drain(5)
    expect(s.display.value.length).toBeGreaterThanOrEqual(360)
  })

  it('bypasses for oversized content', async () => {
    const { s, content, streaming, settle } = setup()
    await settle(() => {
      streaming.value = true
    })
    await settle(() => {
      content.value = 'x'.repeat(20_001)
    })
    expect(s.display.value).toBe('x'.repeat(20_001))
  })
})
