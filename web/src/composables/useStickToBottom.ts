import { onScopeDispose, ref, watch, type Ref } from 'vue'

/**
 * Stick-to-bottom scrolling for streaming message lists: while the user
 * stays within `threshold` px of the bottom, growing content keeps the view
 * pinned (rAF-batched); scrolling up detaches so reading is never yanked —
 * `pinned` flips false and a "jump to latest" affordance can appear with the
 * `missed` count of content updates that happened while detached.
 */
export function useStickToBottom(el: Ref<HTMLElement | null>, threshold = 120) {
  const pinned = ref(true)
  const missed = ref(0)

  function onScroll() {
    const root = el.value
    if (!root) return
    pinned.value = root.scrollHeight - root.scrollTop - root.clientHeight < threshold
    if (pinned.value) missed.value = 0
  }

  /** Call after any content growth (new items, streaming chunks). */
  function follow() {
    if (!pinned.value) {
      missed.value++
      return
    }
    // rAF batches with the DOM mutation that grew the list.
    requestAnimationFrame(() => {
      const root = el.value
      if (root && pinned.value) root.scrollTop = root.scrollHeight
    })
  }

  /** Explicit re-attach (jump button, session switch). */
  function jumpToLatest() {
    pinned.value = true
    missed.value = 0
    requestAnimationFrame(() => {
      const root = el.value
      if (root) root.scrollTop = root.scrollHeight
    })
  }

  watch(
    el,
    (root, prev, onCleanup) => {
      prev?.removeEventListener('scroll', onScroll)
      root?.addEventListener('scroll', onScroll, { passive: true })
      onCleanup(() => root?.removeEventListener('scroll', onScroll))
    },
    { immediate: true },
  )

  onScopeDispose(() => el.value?.removeEventListener('scroll', onScroll))

  return { pinned, missed, follow, jumpToLatest }
}
