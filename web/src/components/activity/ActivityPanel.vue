<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch, type Component } from 'vue'
import { useI18n } from 'vue-i18n'
import { CalendarClock, CircleSlash, ListChecks, Terminal, TriangleAlert, Users, X } from 'lucide-vue-next'
import { useActivityStore } from '../../stores/activity'
import WSkeleton from '../ui/WSkeleton.vue'
import WButton from '../ui/WButton.vue'
import TodoTab from './TodoTab.vue'
import BackgroundTab from './BackgroundTab.vue'
import SubagentTab from './SubagentTab.vue'
import ScheduledTab from './ScheduledTab.vue'

// The anchored popover's card (UIUX §3 option A): tablist (Todo /
// Background / Subagents / Scheduled, each with a count) + the data state
// machine (loading → ready | empty-per-tab | error+retry | unavailable).
// Rendered inside ActivityTrigger's teleported, fixed-position shell; the
// trigger owns open/close and focus return.
const props = defineProps<{ sid: string }>()
const emit = defineEmits<{ close: []; compose: [] }>()

const { t } = useI18n()
const activity = useActivityStore()

type TabId = 'todo' | 'background' | 'subagents' | 'scheduled'
// testid: the Subagents tab button carries the contract id "sub-tab"
// (§4.2); the other tabs address by data-tab like the tablist tests do.
const TABS: Array<{ id: TabId; labelKey: string; icon: Component; testid?: string }> = [
  { id: 'todo', labelKey: 'activity.tabTodo', icon: ListChecks },
  { id: 'background', labelKey: 'activity.tabBackground', icon: Terminal },
  { id: 'subagents', labelKey: 'activity.subagents', icon: Users, testid: 'sub-tab' },
  { id: 'scheduled', labelKey: 'activity.tabScheduled', icon: CalendarClock },
]

// Background is the live tab (the badge lives there); reopening always
// starts on it so a failure lands in front of the user.
const tab = ref<TabId>('background')

const status = computed(() => activity.statusOf(props.sid))
const state = computed(() => activity.stateOf(props.sid))

function tabCount(id: TabId): string {
  if (id === 'todo') {
    const p = activity.todoProgressOf(props.sid)
    return p.total ? `${p.done}/${p.total}` : ''
  }
  if (id === 'background') {
    const n = activity.backgroundCountOf(props.sid)
    return n ? String(n) : ''
  }
  if (id === 'subagents') {
    // Only still-active children count (running + waiting); finished rows
    // stay listed without a badge.
    const n = activity.subagentCountOf(props.sid)
    return n ? String(n) : ''
  }
  const n = activity.cronCountOf(props.sid)
  return n ? String(n) : ''
}

/* ------------------------- data lifecycle hooks ---------------------------- */

// First open (or a session switch while open) with no data yet: schedule a
// refresh. In demo mode this is what seeds the empty snapshot — without it
// the panel would sit in a perpetual loading state.
function ensureLoaded(): void {
  if (props.sid && activity.statusOf(props.sid) === 'loading') activity.refresh(props.sid)
}

// Opening the Background tab marks the current failed set viewed — the
// danger badge resets; anything failing afterwards counts anew.
function markViewed(): void {
  if (props.sid && tab.value === 'background') activity.markViewed(props.sid)
}

onMounted(() => {
  ensureLoaded()
  markViewed()
  updateTabScroll()
  if (typeof ResizeObserver !== 'undefined') {
    tabResizeObserver = new ResizeObserver(() => updateTabScroll())
    if (tablistEl.value) tabResizeObserver.observe(tablistEl.value)
  }
})
onUnmounted(() => {
  tabResizeObserver?.disconnect()
  tabResizeObserver = null
})
watch(() => props.sid, () => {
  ensureLoaded()
  markViewed()
})
watch(tab, markViewed)

function retry(): void {
  void activity.fetchNow(props.sid)
}

// The Scheduled tab carries a warning glyph while this session is capped
// (UIUX §4.3); the persistent bar (and its "send a message" action) lives
// inside the tab. The cap toast is owned by ActivityTrigger — it must fire
// with the panel closed too.
const wakeCapped = computed(() => activity.snapshotOf(props.sid)?.wake.capped ?? false)

/* ------------------------------ tab keyboard ------------------------------ */

// ARIA APG tabs: roving tabindex, ←/→ cycle, Home/End jump (rs-tabs model).
const tablistEl = ref<HTMLElement | null>(null)

function onTablistKeydown(e: KeyboardEvent): void {
  const ids = TABS.map((x) => x.id)
  const i = ids.indexOf(tab.value)
  let next: number | null = null
  if (e.key === 'ArrowRight') next = (i + 1) % ids.length
  else if (e.key === 'ArrowLeft') next = (i - 1 + ids.length) % ids.length
  else if (e.key === 'Home') next = 0
  else if (e.key === 'End') next = ids.length - 1
  if (next == null) return
  e.preventDefault()
  tab.value = ids[next]
  const target = next
  void nextTick(() => {
    tablistEl.value
      ?.querySelector<HTMLElement>(`[data-tab="${ids[target]}"]`)
      ?.focus()
  })
}

/* --------------------------- tab row overflow ------------------------------ */

// The tab row scrolls horizontally once the four labels stop fitting (the
// w375 audit caught en "Subagents" squashed to "Subagen" — tabs are
// flex children and used to shrink instead of overflowing). The end masks
// only render on the sides that actually have content beyond the edge:
// data-scroll is 'none' | 'start' | 'mid' | 'end'.
const tabScroll = ref<'none' | 'start' | 'mid' | 'end'>('none')

function updateTabScroll(): void {
  const el = tablistEl.value
  if (!el) return
  const overflow = el.scrollWidth - el.clientWidth
  if (overflow <= 1) {
    tabScroll.value = 'none'
    return
  }
  const atStart = el.scrollLeft <= 1
  const atEnd = el.scrollLeft >= overflow - 1
  tabScroll.value = atStart ? 'start' : atEnd ? 'end' : 'mid'
}

// Counts arrive asynchronously and can grow a tab past the fold; a
// ResizeObserver on the row catches both viewport and content changes.
// (Test DOMs may not implement it — degrade to the scroll handler alone.)
let tabResizeObserver: ResizeObserver | null = null
</script>

<template>
  <div class="act-pop-card" role="dialog" aria-modal="false" :aria-label="t('activity.title')">
    <div class="act-head">
      <div
        ref="tablistEl"
        class="act-tabs"
        role="tablist"
        :data-scroll="tabScroll"
        :aria-label="t('activity.title')"
        @keydown="onTablistKeydown"
        @scroll.passive="updateTabScroll"
      >
        <button
          v-for="tb in TABS"
          :id="`act-tab-${tb.id}`"
          :key="tb.id"
          class="act-tab"
          :class="{ active: tab === tb.id }"
          type="button"
          role="tab"
          :data-tab="tb.id"
          :data-testid="tb.testid"
          :aria-selected="tab === tb.id"
          :tabindex="tab === tb.id ? 0 : -1"
          @click="tab = tb.id"
        >
          <component :is="tb.icon" :size="13" aria-hidden="true" />
          {{ t(tb.labelKey) }}
          <TriangleAlert
            v-if="tb.id === 'scheduled' && wakeCapped"
            :size="12"
            class="act-tab-warn"
            aria-hidden="true"
          />
          <span v-if="tabCount(tb.id)" class="act-tab-count">{{ tabCount(tb.id) }}</span>
        </button>
      </div>
      <button
        class="act-close"
        type="button"
        :aria-label="t('common.close')"
        @click="emit('close')"
      >
        <X :size="14" aria-hidden="true" />
      </button>
    </div>

    <p v-if="state.stale && status === 'ready'" class="act-stale" role="status">
      {{ t('activity.staleNotice') }}
    </p>

    <div class="act-body">
      <div v-if="status === 'loading'" class="act-loading" role="status" aria-busy="true">
        <span class="sr-only">{{ t('activity.loadingSr') }}</span>
        <WSkeleton variant="rect" height="42px" :repeat="3" />
      </div>
      <div v-else-if="status === 'unavailable'" class="act-empty">
        <CircleSlash :size="16" aria-hidden="true" />
        <span>{{ t('activity.unavailable') }}</span>
      </div>
      <div v-else-if="status === 'error'" class="act-error" role="alert">
        <p class="act-error-text">
          {{ t('activity.loadFailed') }} {{ state.error }}
        </p>
        <WButton variant="secondary" size="sm" @click="retry">{{ t('common.retry') }}</WButton>
      </div>
      <template v-else>
        <div
          v-show="tab === 'todo'"
          class="act-tabpanel"
          role="tabpanel"
          aria-labelledby="act-tab-todo"
        >
          <TodoTab :sid="props.sid" />
        </div>
        <div
          v-show="tab === 'background'"
          class="act-tabpanel"
          role="tabpanel"
          aria-labelledby="act-tab-background"
        >
          <BackgroundTab :sid="props.sid" />
        </div>
        <div
          v-show="tab === 'subagents'"
          class="act-tabpanel"
          role="tabpanel"
          aria-labelledby="act-tab-subagents"
        >
          <SubagentTab :sid="props.sid" />
        </div>
        <div
          v-show="tab === 'scheduled'"
          class="act-tabpanel"
          role="tabpanel"
          aria-labelledby="act-tab-scheduled"
        >
          <ScheduledTab :sid="props.sid" @compose="emit('compose')" />
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.act-pop-card {
  display: flex;
  flex-direction: column;
  /* 440px (was 400) since the Subagents tab landed (§4.2): the row's
   * badge + name + status + elapsed + tokens column needs the width. */
  width: min(440px, calc(100vw - 16px));
  max-height: 65vh;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-overlay);
  overflow: hidden;
}
/* Narrow screens get a slightly taller sheet — still an anchored popover,
 * never a bottom sheet (UIUX §3 A narrow-screen rule). */
@media (max-width: 640px) {
  .act-pop-card {
    max-height: 70vh;
  }
}
.act-head {
  display: flex;
  align-items: stretch;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3) 0;
  border-bottom: 1px solid var(--border);
}
.act-tabs {
  display: flex;
  gap: var(--space-1);
  flex: 1;
  min-width: 0;
  overflow-x: auto;
  /* End masks are pure alpha fades (no colors): each side only masks while
   * content actually extends past it — see the data-scroll rules below. */
  scrollbar-width: thin;
}
/* rs-tab visual language (WorkspaceSidebar precedent) with counts. */
.act-tab {
  /* Never shrink (w375 audit): flex children used to compress mid-word —
   * "Subagents" rendered as "Subagen". Tabs keep their natural width and
   * the row scrolls instead. */
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: var(--space-2) var(--space-3);
  border: none;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: var(--muted);
  font: inherit;
  font-size: var(--text-xs);
  white-space: nowrap;
  cursor: pointer;
}
/* Overflow affordances: fade the side(s) that can scroll further. Mask
   gradients only use the color's alpha channel, so an opaque token
   (var(--accent)) drives the mask without a bare hex. */
.act-tabs[data-scroll='start'] {
  -webkit-mask-image: linear-gradient(
    to right,
    var(--accent) 0,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
  mask-image: linear-gradient(
    to right,
    var(--accent) 0,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
}
.act-tabs[data-scroll='mid'] {
  -webkit-mask-image: linear-gradient(
    to right,
    transparent 0,
    var(--accent) 18px,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
  mask-image: linear-gradient(
    to right,
    transparent 0,
    var(--accent) 18px,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
}
.act-tabs[data-scroll='end'] {
  -webkit-mask-image: linear-gradient(
    to left,
    var(--accent) 0,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
  mask-image: linear-gradient(
    to left,
    var(--accent) 0,
    var(--accent) calc(100% - 18px),
    transparent 100%
  );
}
.act-tab.active {
  color: var(--text);
  border-bottom-color: var(--brand);
}
.act-tab:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: -2px;
}
.act-tab-count {
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: var(--radius-full);
  background: var(--brand-soft);
  color: var(--brand-text);
  font-size: 10px;
  line-height: 16px;
  text-align: center;
}
.act-tab-warn {
  color: var(--warn-text);
}
.act-close {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  margin-top: var(--space-1);
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
}
.act-close:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.act-close:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.act-stale {
  margin: 0;
  padding: var(--space-2) var(--space-4);
  border-bottom: 1px solid var(--border);
  background: var(--warn-soft);
  color: var(--warn-text);
  font-size: var(--text-xs);
}
.act-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.act-tabpanel {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}
.act-tabpanel > * {
  flex: 1;
  min-height: 0;
}
.act-loading {
  padding: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.act-error {
  padding: var(--space-5);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-4);
}
.act-error-text {
  margin: 0;
  color: var(--danger-text);
  font-size: var(--text-sm);
  overflow-wrap: anywhere;
}
</style>
