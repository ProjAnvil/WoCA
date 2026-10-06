import { onScopeDispose, ref, watch } from 'vue'

export interface SmoothStreamOpts {
  /** Store-held source of truth (never lagging semantics). */
  content: () => string
  streaming: () => boolean
  /** Called on every released chunk (stick-to-bottom re-follow). */
  onTick?: () => void
  /** Above this size the smoother bypasses entirely (re-parse fuse). */
  maxDirect?: number
}

const CJK = /[\u{4E00}-\u{9FFF}\u{3040}-\u{30FF}\u{FF00}-\u{FFEF}]/u

function nextChunkLen(s: string, from: number): number {
  const rest = s.slice(from)
  if (!rest) return 0
  if (CJK.test(rest[0])) return 1
  const m = rest.match(/^\s*\S+/)
  return m ? m[0].length : 1
}

/**
 * Display-layer smoothing for streamed assistant text: the store stays the
 * source of truth; this composable releases it to the screen word-by-word
 * (CJK: char-by-char) on a rAF cadence, catching up fast when the network
 * is far ahead. Flushing on stream end is immediate and exact.
 */
export function useSmoothStream(opts: SmoothStreamOpts) {
  const maxDirect = opts.maxDirect ?? 20_000
  const display = ref(opts.content())
  let released = opts.content().length
  let raf = 0
  let last = 0

  function tick(now: number) {
    const total = opts.content()
    const backlog = total.length - released
    if (backlog <= 0) {
      raf = 0
      return
    }
    if (backlog > 300) released = Math.max(released, total.length - 40)
    const interval = backlog > 120 ? 12 : 32
    if (now - last >= interval) {
      last = now
      released += nextChunkLen(total, released)
      display.value = total.slice(0, released)
      opts.onTick?.()
    }
    raf = requestAnimationFrame(tick)
  }

  watch(
    () => opts.content(),
    (total) => {
      if (!opts.streaming() || total.length > maxDirect) {
        released = total.length
        display.value = total
        return
      }
      if (!raf) {
        last = 0
        raf = requestAnimationFrame(tick)
      }
    },
  )

  watch(
    () => opts.streaming(),
    (live) => {
      if (!live) {
        released = opts.content().length
        display.value = opts.content()
        if (raf) {
          cancelAnimationFrame(raf)
          raf = 0
        }
      }
    },
    { immediate: true },
  )

  onScopeDispose(() => raf && cancelAnimationFrame(raf))
  return { display }
}
