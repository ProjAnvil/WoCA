<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Bot, Gauge, Layers, Lightbulb, ShieldCheck } from 'lucide-vue-next'
import { useAgentStore, MODE_DESCRIPTIONS, MODE_LABELS, THINKING_BUDGETS, THINKING_LABELS } from '../../stores/agent'
import { useSessionStore } from '../../stores/sessions'
import { useServerSettingsStore } from '../../stores/serverSettings'
import { useToastStore } from '../../stores/toast'
import { usePreference } from '../../native/preferences'
import { fmtTokens, fmtDuration } from '../../lib/format'
import type { ContextCategory, PermissionMode, ThinkingLevel } from '../../types'

// The composer's chip toolbar: permission mode / model / thinking level /
// context usage (with the cost-transparency popover) / quota. Each chip
// toggles its own popover; state lives in the stores so ⌘K and the
// composer stay in sync.
const { t } = useI18n()
const store = useSessionStore()
const agent = useAgentStore()
const catalog = useServerSettingsStore()
const toast = useToastStore()

type CtlPop = '' | 'mode' | 'thinking' | 'quota' | 'model'
const pop = ref<CtlPop>('')

/* ------------------------- single-line scroll row -------------------------- */

// Audit 07: the toolbar must be one horizontal scroll row (wrapped chips
// stacked over the transcript on narrow shells). The fade mask only makes
// sense when the row actually overflows, so .scrollable is toggled here —
// chip labels are reactive (mode/model/quota/context), so every wrap is
// observed, not just the toolbar's own box.
const toolbarEl = ref<HTMLElement | null>(null)
const scrollable = ref(false)
let chipsRO: ResizeObserver | undefined

function syncScrollable() {
  const el = toolbarEl.value
  scrollable.value = !!el && el.scrollWidth - el.clientWidth > 1
}

/* --------------------------- popover anchoring ----------------------------- */

// Popovers anchor fixed above their chip (WDropdown idiom): the chip row is
// a scroll container, and a scroll container clips absolutely positioned
// children — including popovers opening above it. The chip is measured when
// its popover opens (chips cannot move while one is open: the shared
// backdrop covers the row), and the rendered width re-clamps the left edge
// so nothing crosses the viewport (12px guard both sides).
const popStyle = ref<Record<string, string>>({})
const ctxStyle = ref<Record<string, string>>({})
const popEl = ref<HTMLElement | null>(null)
const ctxEl = ref<HTMLElement | null>(null)

function wrapOf(e: Event): HTMLElement | null {
  return (e.currentTarget as HTMLElement | null)?.closest('.ctl-wrap') ?? null
}

async function placePop(
  wrap: HTMLElement | null,
  style: typeof popStyle,
  card: typeof popEl,
): Promise<void> {
  if (!wrap) return
  const r = wrap.getBoundingClientRect()
  const left = Math.max(12, Math.min(r.left, window.innerWidth - 12))
  style.value = {
    position: 'fixed',
    left: `${left}px`,
    bottom: `${window.innerHeight - r.top + 6}px`,
  }
  await nextTick()
  const w = card.value?.offsetWidth ?? 0
  if (w && left + w > window.innerWidth - 12) {
    style.value = { ...style.value, left: `${Math.max(12, window.innerWidth - w - 12)}px` }
  }
}

// Polite announcement of mode switches (§4.2): no toast — switches are
// frequent; failures keep the existing toast.error path below.
const modeAnnouncement = ref('')

function togglePop(which: CtlPop, e?: Event) {
  const closing = pop.value === which
  pop.value = closing ? '' : which
  if (!closing && e) void placePop(wrapOf(e), popStyle, popEl)
}

/* ----------------------- preferred model (catalog pick) ------------------ */

// The preferred model: picked from the console-configured catalog only
// (the bridge is never hard-bound to one provider — switching verifies
// with the catalog authority and applies from the next model call).
// Persisted through the preferences adapter so the native client keeps it
// in its app storage, not stray WebView localStorage.
const preferredId = usePreference<string>('woca.pref.model', '')

// One-time migration from the legacy key this panel used to write directly
// (bypassing the adapter): seed the canonical key once, then leave the
// legacy value alone.
if (!preferredId.value) {
  const legacy = window.localStorage.getItem('woca.pref.modelPreset')
  if (legacy) preferredId.value = legacy
}

const currentModelName = computed(
  () =>
    catalog.models.find((m) => m.id === preferredId.value)?.name ||
    catalog.models[0]?.name ||
    agent.state?.model ||
    '',
)

async function pickModel(id: string) {
  preferredId.value = id
  pop.value = ''
  await catalog.applyModel(id)
  await agent.load()
}

async function pickMode(mode: PermissionMode) {
  try {
    // Session-scoped (§4.2): the switch applies to the active session's
    // policy only — the popover's note line says so.
    await agent.setMode(mode, store.activeId || undefined)
  } catch (e) {
    toast.error(e)
    return
  }
  modeAnnouncement.value = t('sessions.commands.permissionSwitched', {
    label: MODE_LABELS[mode] ?? mode,
    mode,
  })
  pop.value = ''
}

async function pickThinking(level: ThinkingLevel) {
  try {
    await agent.setThinking(level)
  } catch (e) {
    toast.error(e)
    return
  }
  pop.value = ''
}

// Refresh the quota once before expanding the chip's popover.
async function openQuota(e: Event) {
  if (pop.value === 'quota') {
    pop.value = ''
    return
  }
  pop.value = 'quota'
  void placePop(wrapOf(e), popStyle, popEl)
  void agent.refreshQuota()
}

/* --------------------------- context indicator ---------------------------- */

const ctxHover = ref(false)
const ctxPinned = ref(false)
const ctxOpen = computed(() => ctxPinned.value || ctxHover.value)
let ctxLeaveTimer: number | undefined
let ctxOpenTimer: number | undefined
let ctxFetchedAt = 0

const ctxAvailable = computed(() => store.activeItems.length > 0)

function ctxEnter(e: Event) {
  if (!ctxAvailable.value) return
  const wrap = wrapOf(e)
  window.clearTimeout(ctxLeaveTimer)
  // 300ms dwell before the popover opens; the usage request itself is
  // throttled to once per 30s so sweeping the toolbar cannot fan out a
  // burst of /context calls.
  window.clearTimeout(ctxOpenTimer)
  ctxOpenTimer = window.setTimeout(() => {
    ctxHover.value = true
    void placePop(wrap, ctxStyle, ctxEl)
    if (!store.contextLoading && Date.now() - ctxFetchedAt > 30_000) {
      ctxFetchedAt = Date.now()
      void store.refreshContext()
    }
  }, 300)
}
function ctxLeave() {
  window.clearTimeout(ctxOpenTimer)
  window.clearTimeout(ctxLeaveTimer)
  ctxLeaveTimer = window.setTimeout(() => (ctxHover.value = false), 150)
}

// Click pins the popover open (keyboard/touch parity with hover), fires the
// throttled refreshes, and the next click (or Esc / backdrop) closes it.
function toggleCtx(e: Event) {
  if (!ctxAvailable.value) return
  if (ctxPinned.value) {
    ctxPinned.value = false
    return
  }
  ctxPinned.value = true
  void placePop(wrapOf(e), ctxStyle, ctxEl)
  if (!store.contextLoading && Date.now() - ctxFetchedAt > 30_000) {
    ctxFetchedAt = Date.now()
    void store.refreshContext()
  }
  if (!agent.quotaLoading) void agent.refreshQuota()
}

/* ---------------------------- cost transparency ---------------------------- */

// Cumulative usage comes from /api/quota's session block (process lifetime:
// requests + input/output/cache tokens). The chat-session span is derived
// from assistant timestamps — no backend field exists for it.
const RING_C = 2 * Math.PI * 26

const sessionUsage = computed(() => agent.quota?.session ?? null)

const ringTone = computed(() => (ctxWindowPct.value < 60 ? 'ok' : ctxWindowPct.value < 85 ? 'warn' : 'danger'))

const sessionSpanLabel = computed(() => {
  const stamps = store.activeItems
    .map((i) => (i.kind === 'assistant' ? i.ts ?? 0 : 0))
    .filter(Boolean)
  if (stamps.length < 2) return ''
  return fmtDuration(Math.max(...stamps) - Math.min(...stamps))
})

// The provider-reported total of the most recent model call (input + cache
// reads + cache writes) — the exact counterpart of the estimate.
const ctxExact = computed(() => {
  const lr = store.contextInfo?.last_request
  return lr ? lr.input_tokens + lr.cache_read_tokens + lr.cache_write_tokens : 0
})

const ctxShort = computed(() => {
  // The empty value reads as "Not enabled" instead of a bare dash (audit 07:
  // the dash placeholder was unreadable as a status).
  if (!store.contextInfo) return store.contextLoading ? '…' : t('chat.statusDisabled')
  return ctxExact.value ? fmtTokens(ctxExact.value) : `~${fmtTokens(store.contextInfo.estimated_total)}`
})

const ctxWindowPct = computed(() => {
  const info = store.contextInfo
  return info?.context_window ? Math.min(100, (info.estimated_total / info.context_window) * 100) : 0
})

const ctxCompactPct = computed(() => {
  const info = store.contextInfo
  return info?.context_window && info.compact_threshold
    ? Math.min(100, (info.compact_threshold / info.context_window) * 100)
    : 0
})

function ctxCatPct(c: ContextCategory): string {
  const total = store.contextInfo?.estimated_total ?? 0
  return total ? `${Math.max(2, (c.tokens / total) * 100).toFixed(1)}%` : '0%'
}

const modeList = Object.entries(MODE_LABELS) as [PermissionMode, string][] as [PermissionMode, string][]

// Quota chip: an unavailable quota (the store's quotaNone flag — derived
// from the underlying quota state, not from comparing the rendered label)
// shows the same "Not enabled" placeholder as the context chip instead of a
// bare dash.
const quotaNone = computed(() => agent.quotaNone)
const quotaLabel = computed(() => (quotaNone.value ? t('chat.statusDisabled') : agent.quotaShort))

onMounted(() => {
  if (!preferredId.value && catalog.models.length) {
    preferredId.value = catalog.models[0].id
  }
  if (typeof ResizeObserver !== 'undefined') {
    chipsRO = new ResizeObserver(syncScrollable)
    if (toolbarEl.value) {
      chipsRO.observe(toolbarEl.value)
      for (const el of toolbarEl.value.querySelectorAll<HTMLElement>('.ctl-wrap')) chipsRO.observe(el)
    }
  }
  syncScrollable()
  void agent.load()
  void agent.refreshQuota()
  void catalog.ensureModels()
})

onBeforeUnmount(() => chipsRO?.disconnect())
</script>

<template>
  <!-- Chat-input toolbar: permission mode / thinking level / remaining quota.
       The overflow fade mask is dropped while any popover is open: masks
       paint fixed-position descendants too, and the popovers anchor fixed
       above their chip (placePop). -->
  <div
    ref="toolbarEl"
    class="input-toolbar"
    :class="{ scrollable: scrollable && !pop && !ctxOpen }"
  >
    <div class="ctl-wrap">
      <button
        class="ctl-chip"
        :class="{ open: pop === 'mode' }"
        :title="t('chat.switchModeTitle')"
        :aria-expanded="pop === 'mode'"
        data-testid="mode-chip"
        @click="togglePop('mode', $event)"
      >
        <ShieldCheck :size="13" />
        <!-- Keyed label: the fade transition cross-fades mode switches (240ms). -->
        <Transition name="mode-fade" mode="out-in">
          <span :key="agent.modeLabel">{{ agent.modeLabel }}</span>
        </Transition>
      </button>
      <!-- Polite switch announcements live next to the chip they describe. -->
      <span class="sr-only" aria-live="polite">{{ modeAnnouncement }}</span>
      <div v-if="pop === 'mode'" ref="popEl" class="ctl-pop" :style="popStyle">
        <div class="ctl-pop-header">{{ t('chat.permissionMode') }}</div>
        <div class="ctl-pop-note-line" data-testid="mode-session-note">{{ t('chat.modeSessionNote') }}</div>
        <button
          v-for="[m, label] in modeList"
          :key="m"
          class="ctl-opt"
          :class="{ active: agent.mode === m }"
          type="button"
          @click="pickMode(m)"
        >
          <span class="ctl-opt-name"
            >{{ label }}<span v-if="agent.mode === m" class="ctl-opt-session">{{ t('chat.modeSessionBadge') }}</span></span
          >
          <span class="ctl-opt-desc">{{ MODE_DESCRIPTIONS[m] }}</span>
        </button>
      </div>
    </div>

    <div v-if="catalog.models.length" class="ctl-wrap">
      <button
        class="ctl-chip"
        :class="{ open: pop === 'model' }"
        :title="t('chat.modelChange')"
        :aria-expanded="pop === 'model'"
        @click="togglePop('model', $event)"
      >
        <Bot :size="13" />
        <span>{{ currentModelName }}</span>
      </button>
      <div v-if="pop === 'model'" ref="popEl" class="ctl-pop" :style="popStyle">
        <div class="ctl-pop-header">{{ t('chat.modelPick') }}</div>
        <button
          v-for="m in catalog.models"
          :key="m.id"
          class="ctl-opt"
          :class="{ active: (preferredId || catalog.models[0]?.id) === m.id }"
          type="button"
          @click="pickModel(m.id)"
        >
          <span class="ctl-opt-name">{{ m.name }}<span class="ctl-opt-key">{{ m.model }}</span></span>
          <span class="ctl-opt-desc">{{ m.provider }}</span>
        </button>
      </div>
    </div>

    <div class="ctl-wrap">
      <button
        class="ctl-chip"
        :class="{ open: pop === 'thinking', disabled: !agent.thinkingSupported }"
        :title="
          agent.thinkingSupported
            ? t('chat.thinkingChange')
            : t('chat.thinkingUnsupported', { model: agent.state?.model ?? t('chat.currentModel') })
        "
        :aria-expanded="pop === 'thinking'"
        :disabled="!agent.thinkingSupported"
        @click="agent.thinkingSupported && togglePop('thinking', $event)"
      >
        <Lightbulb :size="13" />
        <span>{{ t('chat.thinkingLabel') }}·{{ agent.thinkingSupported ? agent.thinkingLabel : t('chat.unsupported') }}</span>
      </button>
      <div v-if="pop === 'thinking'" ref="popEl" class="ctl-pop" :style="popStyle">
        <div class="ctl-pop-header">
          {{ t('chat.thinkingLevel') }}
          <span v-if="agent.thinkingNote" class="ctl-pop-note">{{ agent.thinkingNote }}</span>
        </div>
        <button
          v-for="level in (['off', ...agent.thinkingLevels] as ThinkingLevel[])"
          :key="level"
          class="ctl-opt"
          :class="{ active: agent.thinkingLevel === level }"
          type="button"
          @click="pickThinking(level)"
        >
          <span class="ctl-opt-name"
            >{{ THINKING_LABELS[level]
            }}<span class="ctl-opt-key">{{
              THINKING_BUDGETS[level] ? `${THINKING_BUDGETS[level]} tokens` : t('chat.off')
            }}</span></span
          >
        </button>
      </div>
    </div>

    <div
      class="ctl-wrap"
      @mouseenter="ctxEnter"
      @mouseleave="ctxLeave"
      @keydown.esc="ctxPinned = false"
    >
      <button
        class="ctl-chip"
        :class="{ open: ctxOpen, disabled: !ctxAvailable }"
        type="button"
        :title="ctxAvailable ? t('chat.ctxUsageTitle') : t('chat.ctxUsageUnavailable')"
        :aria-expanded="ctxOpen"
        aria-haspopup="dialog"
        :disabled="!ctxAvailable"
        @click="toggleCtx($event)"
      >
        <Layers :size="13" />
        <span>{{ t('chat.context') }} {{ ctxAvailable ? ctxShort : t('chat.statusDisabled') }}</span>
      </button>
      <div
        v-if="ctxOpen"
        ref="ctxEl"
        class="ctl-pop ctx-pop"
        :style="ctxStyle"
        @mouseenter="ctxEnter"
        @mouseleave="ctxLeave"
      >
        <div class="ctl-pop-header">
          {{ t('chat.ctxBreakdown') }}
          <span v-if="store.contextInfo?.model" class="ctl-pop-note">{{ store.contextInfo.model }}</span>
        </div>
        <div class="ctx-body">
          <div v-if="store.contextError" class="ctx-error">{{ store.contextError }}</div>
          <template v-else-if="store.contextInfo">
            <div v-if="ctxExact" class="ctx-exact">
              <span class="ctx-exact-num">{{ fmtTokens(ctxExact) }}</span>
              <span class="ctx-exact-detail">
                {{
                  t('chat.ctxExactDetail', {
                    input: fmtTokens(store.contextInfo.last_request!.input_tokens),
                    cacheRead: fmtTokens(store.contextInfo.last_request!.cache_read_tokens),
                    cacheWrite: fmtTokens(store.contextInfo.last_request!.cache_write_tokens),
                  })
                }}
              </span>
            </div>
            <div v-else class="ctx-exact-detail">{{ t('chat.ctxNoExact') }}</div>
            <div class="ctx-estimate">
              {{ t('chat.ctxEstimate') }} <b>~{{ fmtTokens(store.contextInfo.estimated_total) }}</b> tokens
            </div>
            <div v-if="store.contextInfo.context_window" class="ctx-window">
              <svg
                class="ctx-ring"
                viewBox="0 0 60 60"
                role="img"
                :aria-label="t('chat.ctxRingLabel', { pct: ctxWindowPct.toFixed(0) })"
              >
                <circle class="ctx-ring-track" cx="30" cy="30" r="26" />
                <circle
                  class="ctx-ring-fill"
                  :class="ringTone"
                  cx="30"
                  cy="30"
                  r="26"
                  :stroke-dasharray="RING_C"
                  :stroke-dashoffset="RING_C * (1 - ctxWindowPct / 100)"
                />
                <text x="30" y="34" class="ctx-ring-text">{{ ctxWindowPct.toFixed(0) }}%</text>
              </svg>
              <div class="ctx-window-cols">
                <div class="ctx-bar">
                  <div class="ctx-bar-fill" :style="{ width: ctxWindowPct + '%' }"></div>
                  <div
                    v-if="ctxCompactPct"
                    class="ctx-bar-mark"
                    :style="{ left: ctxCompactPct + '%' }"
                    :title="t('chat.compactThreshold')"
                  ></div>
                </div>
                <div class="ctx-bar-label">
                  {{
                    t('chat.ctxWindowLabel', {
                      used: fmtTokens(store.contextInfo.estimated_total),
                      total: fmtTokens(store.contextInfo.context_window),
                      pct: ctxWindowPct.toFixed(0),
                    })
                  }}
                </div>
              </div>
            </div>
            <div v-if="store.contextInfo.compact_threshold" class="ctx-compact">
              {{ t('chat.compactThreshold') }} {{ fmtTokens(store.contextInfo.compact_threshold) }} tokens
            </div>
            <div class="ctx-cats">
              <div v-for="c in store.contextInfo.categories" :key="c.key" class="ctx-cat">
                <div class="ctx-cat-head">
                  <span class="ctx-cat-label">
                    {{ c.label }}<span v-if="c.detail" class="ctx-cat-detail">{{ c.detail }}</span>
                  </span>
                  <span class="ctx-cat-tokens">
                    ~{{ fmtTokens(c.tokens) }}<span v-if="c.count"> · {{ c.count }}</span>
                  </span>
                </div>
                <div class="ctx-cat-bar"><div class="ctx-cat-fill" :style="{ width: ctxCatPct(c) }"></div></div>
              </div>
            </div>
            <div v-if="sessionUsage" class="ctx-cumulative">
              <div class="ctx-cum-title">{{ t('chat.ctxCumulative') }}</div>
              <div class="ctx-cum-rows">
                <span>{{ t('chat.ctxReqCount', { count: sessionUsage.requests ?? 0 }) }}</span>
                <span>{{ t('chat.ctxIn', { tokens: fmtTokens(sessionUsage.input_tokens ?? 0) }) }}</span>
                <span>{{ t('chat.ctxOut', { tokens: fmtTokens(sessionUsage.output_tokens ?? 0) }) }}</span>
                <span>{{ t('chat.ctxCacheHit', { tokens: fmtTokens(sessionUsage.cache_read_tokens ?? 0) }) }}</span>
              </div>
              <div class="ctx-cum-note">
                {{ t('chat.ctxCumulativeNote') }}<template v-if="sessionSpanLabel"> · {{ t('chat.ctxSessionSpan', { span: sessionSpanLabel }) }}</template>
              </div>
            </div>
            <div class="ctx-foot">{{ t('chat.ctxFoot') }}</div>
          </template>
          <div v-else class="ctx-foot">{{ t('common.loading') }}</div>
        </div>
      </div>
    </div>

    <div class="ctl-wrap">
      <button
        class="ctl-chip"
        :class="{ open: pop === 'quota' }"
        :title="quotaNone ? t('chat.viewQuota') : agent.quotaShort"
        :aria-expanded="pop === 'quota'"
        @click="openQuota($event)"
      >
        <Gauge :size="13" />
        <span>{{ agent.quotaLoading ? t('chat.querying') : quotaLabel }}</span>
      </button>
      <div v-if="pop === 'quota'" ref="popEl" class="ctl-pop quota-pop" :style="popStyle">
        <div class="ctl-pop-header">
          {{ t('chat.quota') }}
          <button class="ctl-refresh" type="button" @click="agent.refreshQuota()">{{ t('common.refresh') }}</button>
        </div>
        <div class="quota-body">{{ agent.quotaError || agent.quotaText }}</div>
      </div>
    </div>

    <!-- Clicking outside any popover (chips or pinned context) closes it -->
    <div v-if="pop || ctxPinned" class="ctl-backdrop" @click="pop = ''; ctxPinned = false"></div>
  </div>
</template>

<style scoped>
/* Session-scoped mode popover (§4.2): the note line under the header, the
   active option's "this session" suffix badge, and the 240ms label fade. */
.ctl-pop-note-line {
  padding: 2px 12px 8px;
  font-size: var(--text-xs);
  color: var(--muted);
}
.ctl-opt-session {
  margin-inline-start: 6px;
  padding: 1px 6px;
  border-radius: var(--radius-full);
  background: var(--brand-soft);
  color: var(--brand-text);
  font-size: 10.5px;
}
.mode-fade-enter-active,
.mode-fade-leave-active {
  transition: opacity 240ms var(--ease-in-out);
}
.mode-fade-enter-from,
.mode-fade-leave-to {
  opacity: 0;
}
</style>
