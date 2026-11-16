<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Users } from 'lucide-vue-next'
import { useActivityStore } from '../../stores/activity'
import { useSessionStore } from '../../stores/sessions'
import { hitlOrigin, hitlRequestId } from '../../lib/hitl'
import SubagentRow from './SubagentRow.vue'
import TranscriptView from './TranscriptView.vue'
import type { SubagentRun } from '../../types'

// The Subagents tab (§4.1/§4.2): the session's foreground children as an
// in-panel list — NOT a tree; the current graph is main→subagent one level
// and trees waste the 440px popover (grandchild support stays reserved via
// a future +N fold and one indent level). Rows toggle an in-panel readonly
// transcript reveal; waiting rows carry the go-to-approval affordance.
const props = defineProps<{ sid: string }>()

const { t } = useI18n()
const activity = useActivityStore()
const sessions = useSessionStore()

const runs = computed<SubagentRun[]>(() => activity.snapshotOf(props.sid)?.subagents ?? [])

/* ----------------------- running clocks (1s tick) ------------------------- */

// One shared interval drives every row's elapsed label (§4.1 a11y note),
// on the server's clock like BackgroundTab (remote/relay elapsed math).
const clockOffsetMs = computed(() => {
  const s = activity.snapshotOf(props.sid)
  if (!s) return 0
  const parsed = Date.parse(s.server_time)
  return Number.isNaN(parsed) ? 0 : parsed - Date.now()
})

const now = ref(Date.now())
let tick: number | undefined

// The one-second elapsed tick only matters while some row is still
// running or waiting — terminal rows render a frozen duration, so the
// interval (and its per-second re-render of every row) stops when nothing
// is live.
const anyLive = computed(() =>
  runs.value.some((r) => r.state === 'running' || r.state === 'waiting'),
)
watch(anyLive, (live) => {
  if (live && tick === undefined) {
    tick = window.setInterval(() => (now.value = Date.now()), 1000)
  } else if (!live && tick !== undefined) {
    window.clearInterval(tick)
    tick = undefined
  }
}, { immediate: true })
onBeforeUnmount(() => window.clearInterval(tick))

/* --------------------------- expansion (reveal) --------------------------- */

const expanded = ref<ReadonlySet<string>>(new Set())

function toggle(run: SubagentRun): void {
  const next = new Set(expanded.value)
  if (next.has(run.call_id)) next.delete(run.call_id)
  else next.add(run.call_id)
  expanded.value = next
}

/* ------------------------ go-to-approval (§4.1) --------------------------- */

// A waiting child's pause card is located by its origin: child asks carry
// origin = the subagent def (askOrigin, Task 7) — the row's def. The
// affordance closes this popover (the HitlStack's scrim sits above it) and
// focuses the card's testid anchor (hitl-card-${requestId}); with no card
// mapped yet (the pause raced ahead of the snapshot) it still closes the
// panel so the stack is unobstructed.
function gotoApproval(run: SubagentRun): void {
  const cards = sessions.activeHitlCards ?? []
  const card = cards.find((c) => hitlOrigin(c) === run.def)
  sessions.activityPanelOpen = false
  const requestId = card ? hitlRequestId(card) : ''
  void nextTick(() => {
    if (!requestId) return
    const el = document.querySelector<HTMLElement>(
      `[data-testid="hitl-card-${CSS.escape(requestId)}"]`,
    )
    el?.scrollIntoView({ block: 'center' })
    // Focus lands inside the card (its first actionable button) so keyboard
    // users land where the decision is; the card root is the fallback.
    ;(el?.querySelector<HTMLElement>('button') ?? el)?.focus()
  })
}

/* ----------------------------- row keyboard ------------------------------- */

// ↑/↓ rove between rows, Enter expands the focused row, Esc collapses it
// (copying BackgroundTab.vue:184-200; Esc stops propagation when consumed
// so the popover's own Esc-to-close fires only when nothing is expanded).
const listEl = ref<HTMLElement | null>(null)

function onListKeydown(e: KeyboardEvent): void {
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter' && e.key !== 'Escape') return
  const rowEls = Array.from(listEl.value?.querySelectorAll<HTMLElement>('[data-row-key]') ?? [])
  if (!rowEls.length) return
  const idx = rowEls.indexOf(document.activeElement as HTMLElement)
  if (e.key === 'Enter') {
    if (idx < 0) return // focus is on an inner control: native Enter applies
    e.preventDefault()
    const run = runs.value.find((x) => x.call_id === rowEls[idx].dataset.rowKey)
    if (run) toggle(run)
    return
  }
  if (e.key === 'Escape') {
    if (idx < 0) return
    const run = runs.value.find((x) => x.call_id === rowEls[idx].dataset.rowKey)
    if (run && expanded.value.has(run.call_id)) {
      e.preventDefault()
      e.stopPropagation()
      toggle(run)
    }
    return
  }
  e.preventDefault()
  const from = idx < 0 ? 0 : idx
  const delta = e.key === 'ArrowDown' ? 1 : -1
  rowEls[(from + delta + rowEls.length) % rowEls.length]?.focus()
}
</script>

<template>
  <div class="act-tab-body">
    <div v-if="runs.length" ref="listEl" class="sub-list" role="list" @keydown="onListKeydown">
      <div v-for="run in runs" :key="run.call_id" role="listitem">
        <SubagentRow
          :run="run"
          :expanded="expanded.has(run.call_id)"
          :now="now"
          :clock-offset-ms="clockOffsetMs"
          @toggle="toggle(run)"
          @goto="gotoApproval(run)"
        />
        <div class="sub-transcript-reveal" :class="{ open: expanded.has(run.call_id) }">
          <div class="sub-transcript-reveal-inner" :aria-hidden="expanded.has(run.call_id) ? undefined : 'true'" :inert="!expanded.has(run.call_id)">
            <TranscriptView v-if="expanded.has(run.call_id)" :run="run" />
          </div>
        </div>
      </div>
    </div>
    <div v-else class="act-empty" data-testid="sub-empty">
      <Users :size="16" aria-hidden="true" />
      <span>{{ t('activity.subagentsEmpty') }}</span>
    </div>
  </div>
</template>

<style scoped>
.sub-list {
  padding: var(--space-2) var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.sub-list > [role='listitem'] {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

/* Reveal (motion.css four-grammar rule): the transcript expands via the
 * grid-rows 0fr↔1fr pattern WCollapseCard uses — height-only, no transform
 * tricks on content. */
.sub-transcript-reveal {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows var(--dur-slow) var(--ease-out);
}
.sub-transcript-reveal.open {
  grid-template-rows: 1fr;
}
.sub-transcript-reveal-inner {
  min-height: 0;
  overflow: hidden;
}
</style>
