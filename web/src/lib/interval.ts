import { onBeforeUnmount } from 'vue'

/**
 * usePolling runs fn immediately and then on a fixed interval, clearing the
 * timer on component unmount — the shared replacement for the hand-rolled
 * `let timer: ReturnType<typeof setInterval> | null` + onBeforeUnmount
 * bookkeeping that had drifted across three components.
 *
 * The callback may be swapped between ticks (callers pass a fresh closure);
 * only the currently armed interval is ever cleared. `stop()` cancels
 * early and is idempotent.
 */
export function usePolling(fn: () => void | Promise<void>, ms: number) {
  let timer: ReturnType<typeof setInterval> | null = null
  const tick = () => {
    void fn()
  }
  function stop() {
    if (timer != null) {
      clearInterval(timer)
      timer = null
    }
  }
  function start() {
    if (timer != null) return
    tick()
    timer = setInterval(tick, ms)
  }
  onBeforeUnmount(stop)
  return { start, stop }
}
