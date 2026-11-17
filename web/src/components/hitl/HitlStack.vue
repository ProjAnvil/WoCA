<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useSessionStore } from '../../stores/sessions'
import { useFocusTrap } from '../../composables/useFocusTrap'
import { hitlOrigin, asksOf, hitlRequestId } from '../../lib/hitl'
import { listGrants } from '../../api/client'
import type { GrantInfo } from '../../types'
import HitlCard from './HitlCard.vue'
import GrantsPanel from '../settings/GrantsPanel.vue'

// The multi-card approval stack (§4.2): a modal overlay replacing
// InterruptModal's render layer for multi-card pauses and approvals kinds.
// Cards stack in parallel (never a serial queue); past three they fold into
// an "N more" reveal; the whole stack scrolls at 84vh.
const { t } = useI18n()
const store = useSessionStore()

const cards = computed(() => store.activeHitlCards ?? [])

// One shared interval drives every card's elapsed label (§4.1 a11y note).
const now = ref(Date.now())
let tick: number | undefined
onMounted(() => {
  tick = window.setInterval(() => (now.value = Date.now()), 1000)
})
onUnmounted(() => window.clearInterval(tick))

// >3 cards fold; the reveal row expands the remainder (grid-rows reveal —
// the folded cards stay in the DOM inside the 0fr container).
const FOLD_AT = 3
const expanded = ref(false)
const visible = computed(() => cards.value.slice(0, FOLD_AT))
const folded = computed(() => cards.value.slice(FOLD_AT))
const hiddenCount = computed(() => Math.max(0, cards.value.length - FOLD_AT))
// Head counter "item n of m": the 1-based index of the last card in view
// (the fold reveal counts too) over the total pending.
const shownCount = computed(() => (expanded.value ? cards.value.length : visible.value.length))

// Modal semantics (WModal precedent): dialog role + focus trap; the
// announcer node lives INSIDE the scrim so live regions are never inert.
const panel = ref<HTMLElement | null>(null)
const active = computed(() => cards.value.length > 0)
useFocusTrap(panel, active)

const announcement = ref('')
watch(
  () => cards.value.map((c) => hitlRequestId(c)).join('|'),
  (ids, prev) => {
    if (!ids || ids === prev) return
    const prevSet = new Set((prev ?? '').split('|').filter(Boolean))
    const fresh = ids.split('|').filter((id) => id && !prevSet.has(id))
    if (!fresh.length) return
    const first = cards.value.find((c) => hitlRequestId(c) === fresh[0])
    const who = first ? hitlOrigin(first) : ''
    const what = first ? (asksOf(first)[0]?.tool_name ?? first.kind) : ''
    announcement.value = [who, what, t('chat.pendingCount', { count: fresh.length })].filter(Boolean).join(' — ')
  },
  { immediate: true },
)

// Session grants footnote: "N granted · manage" opens the grants panel.
const grants = ref<GrantInfo[]>([])
const grantsOpen = ref(false)
async function refreshGrants() {
  if (store.demo) return
  try {
    const resp = await listGrants(store.activeId)
    grants.value = resp.grants ?? []
  } catch {
    grants.value = [] // the footnote is best-effort; the panel re-fetches
  }
}
onMounted(() => void refreshGrants())
</script>

<template>
  <Transition name="panel-pop">
    <div v-if="cards.length" class="overlay hitl-scrim">
      <div
        ref="panel"
        class="hitl-stack"
        role="dialog"
        aria-modal="true"
        :aria-label="t('chat.awaitingConfirm')"
        data-testid="hitl-stack"
        tabindex="-1"
      >
        <!-- polite announcer inside the scrim (never on inert background DOM) -->
        <span class="sr-only" aria-live="polite">{{ announcement }}</span>

        <div class="hitl-stack-head">
          <span class="hitl-stack-title">{{ t('chat.awaitingConfirm') }}</span>
          <span class="hitl-stack-meta">
            <span class="hitl-stack-count" data-testid="hitl-stack-count">{{
              t('chat.cardPosition', { n: shownCount, m: cards.length })
            }}</span>
            <button
              v-if="grants.length"
              class="hitl-grants-chip"
              type="button"
              data-testid="grants-chip"
              @click="grantsOpen = true"
            >
              {{ t('chat.grantedRules', { count: grants.length }) }}
            </button>
          </span>
        </div>

        <TransitionGroup name="hitl-settle" tag="div" class="hitl-stack-list">
          <HitlCard
            v-for="(c, i) in visible"
            :key="c.ns || c.id"
            :card="c"
            :now="now"
            :data-testid="`hitl-card-${hitlRequestId(c)}`"
          />
          <div v-if="hiddenCount" :key="'fold'" class="hitl-fold" :class="{ open: expanded }">
            <div class="hitl-fold-inner" :inert="expanded ? undefined : true" :aria-hidden="expanded ? undefined : `true`">
              <HitlCard
                v-for="(c, i) in folded"
                :key="c.ns || c.id"
                :card="c"
                :now="now"
                :data-testid="`hitl-card-${hitlRequestId(c)}`"
              />
            </div>
          </div>
        </TransitionGroup>

        <button
          v-if="hiddenCount"
          class="hitl-queue-more"
          type="button"
          data-testid="hitl-queue-more"
          :aria-expanded="expanded"
          @click="expanded = !expanded"
        >
          {{ t('chat.pendingCount', { count: hiddenCount }) }}
        </button>
      </div>
    </div>
  </Transition>

  <GrantsPanel :open="grantsOpen" :session="store.activeId" @close="grantsOpen = false" @revoked="void refreshGrants()" />
</template>

<style scoped>
.hitl-scrim {
  background: color-mix(in srgb, var(--bg) 32%, transparent);
}
.hitl-stack {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  width: 520px;
  max-width: 100%;
  max-height: 84vh;
  overflow-y: auto;
  padding: var(--space-2);
  outline: none;
}
.hitl-stack-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 var(--space-1);
}
.hitl-stack-title {
  font-weight: 600;
  color: var(--text);
}
.hitl-stack-meta {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}
.hitl-stack-count {
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.hitl-grants-chip {
  background: none;
  border: none;
  padding: 2px 6px;
  border-radius: var(--radius-full);
  font-size: var(--text-xs);
  color: var(--muted);
  cursor: pointer;
}
.hitl-grants-chip:hover {
  background: var(--brand-soft);
  color: var(--text);
}
.hitl-stack-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
/* The fold reveal (motion.css reveal grammar): grid-rows 0fr↔1fr. */
.hitl-fold {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows var(--dur-slow) var(--ease-out);
}
.hitl-fold.open {
  grid-template-rows: 1fr;
}
.hitl-fold-inner {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-height: 0;
  overflow: hidden;
}
.hitl-queue-more {
  align-self: center;
  background: none;
  border: 1px dashed var(--border);
  border-radius: var(--radius-full);
  padding: 4px 14px;
  font-size: var(--text-xs);
  color: var(--muted);
  cursor: pointer;
}
.hitl-queue-more:hover {
  color: var(--text);
  border-color: var(--muted);
}
/* New cards rise in (320ms, the spec's entrance tier); answered cards
 * settle out one tier faster than the rise (leaves are always quicker). */
.hitl-settle-enter-active {
  transition: opacity 320ms var(--ease-out), transform 320ms var(--ease-out);
}
.hitl-settle-enter-from {
  opacity: 0;
  transform: translateY(12px);
}
.hitl-settle-leave-active {
  transition: opacity 120ms var(--ease-in-out), transform 120ms var(--ease-in-out);
}
.hitl-settle-leave-to {
  opacity: 0;
  transform: translateY(4px);
}
</style>
