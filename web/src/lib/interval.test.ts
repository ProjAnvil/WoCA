import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { usePolling } from './interval'

describe('usePolling', () => {
  it('runs immediately, repeats on the interval, and stops on unmount', async () => {
    vi.useFakeTimers()
    const fn = vi.fn()
    const Host = defineComponent({
      setup() {
        usePolling(fn, 1000).start()
        return () => null
      },
    })
    const w = mount(Host)
    expect(fn).toHaveBeenCalledTimes(1) // immediate first tick

    await vi.advanceTimersByTimeAsync(3000)
    expect(fn).toHaveBeenCalledTimes(4)

    w.unmount()
    await vi.advanceTimersByTimeAsync(5000)
    expect(fn).toHaveBeenCalledTimes(4) // cleared on unmount
    vi.useRealTimers()
  })

  it('stop() cancels early and start() re-arms exactly one interval', async () => {
    vi.useFakeTimers()
    const fn = vi.fn()
    let controls!: ReturnType<typeof usePolling>
    const Host = defineComponent({
      setup() {
        controls = usePolling(fn, 1000)
        return () => null
      },
    })
    const w = mount(Host)
    controls.start()
    controls.start() // idempotent: one armed interval
    await vi.advanceTimersByTimeAsync(1000)
    expect(fn).toHaveBeenCalledTimes(2) // immediate + one tick (not two)

    controls.stop()
    await vi.advanceTimersByTimeAsync(3000)
    expect(fn).toHaveBeenCalledTimes(2)

    controls.start()
    await vi.advanceTimersByTimeAsync(1000)
    expect(fn).toHaveBeenCalledTimes(4) // immediate + one tick again
    w.unmount()
    vi.useRealTimers()
  })
})
