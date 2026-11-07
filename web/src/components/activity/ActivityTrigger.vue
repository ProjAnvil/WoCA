<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ListChecks } from 'lucide-vue-next'
import WIconButton from '../ui/WIconButton.vue'
import ActivityPanel from './ActivityPanel.vue'
import { useSessionStore } from '../../stores/sessions'
import { useActivityStore } from '../../stores/activity'
import { useToastStore } from '../../stores/toast'

// The header entry (UIUX §3 option A): an icon button with a live badge
// (running count, brand; unviewed failures take priority, danger) that
// anchors a non-modal popover below it. The popover reuses WDropdown's
// positioning model (teleport to body, fixed, flip when space runs out)
// but dialog semantics: aria-haspopup="dialog", Esc/outside-close, focus
// returns to the trigger, and a light Tab trap keeps focus inside.
// Always mounted in the chat header, so it also owns the wake-cap rising
// edge toast (UIUX §4.3) — with the panel closed, which is the typical
// case.
const { t } = useI18n()
const sessions = useSessionStore()
const activity = useActivityStore()
const toast = useToastStore()

const emit = defineEmits<{ compose: [] }>()

const panelId = useId()

const sid = computed(() => sessions.activeId)
const open = computed(() => sessions.activityPanelOpen)

const runningCount = computed(() => (sid.value ? activity.runningCountOf(sid.value) : 0))
const unviewedFailures = computed(() =>
  sid.value ? activity.unviewedFailureCountOf(sid.value) : 0,
)
// Badge priority (UIUX §3 A): unviewed failures outrank the running count —
// completions never badge, only "in flight" and "went wrong" persist.
const badgeCount = computed(() =>
  unviewedFailures.value > 0 ? unviewedFailures.value : runningCount.value,
)
const badgeTone = computed<'brand' | 'danger'>(() =>
  unviewedFailures.value > 0 ? 'danger' : 'brand',
)
const triggerLabel = computed(() => {
  if (unviewedFailures.value > 0) {
    return t('activity.triggerLabelFailed', {
      running: runningCount.value,
      failed: unviewedFailures.value,
    })
  }
  if (runningCount.value > 0) {
    return t('activity.triggerLabelRunning', { running: runningCount.value })
  }
  return t('activity.triggerLabel')
})

function toggle(): void {
  sessions.activityPanelOpen = !sessions.activityPanelOpen
}

function close(focusTrigger = false): void {
  sessions.activityPanelOpen = false
  if (focusTrigger) wrapEl.value?.querySelector('button')?.focus()
}

// One-shot brand pulse when the badge changes while the panel is closed
// (UIUX §4.2) — a background event arrived and nobody was looking.
const pulse = ref(false)
let pulseTimer: number | undefined

watch(badgeCount, (n, was) => {
  if (n === was || open.value) return
  pulse.value = false
  window.clearTimeout(pulseTimer)
  void nextTick(() => {
    pulse.value = true
    pulseTimer = window.setTimeout(() => (pulse.value = false), 240)
  })
})

/* ------------------------------ positioning -------------------------------- */

const wrapEl = ref<HTMLElement | null>(null)
const panelEl = ref<HTMLElement | null>(null)
const popStyle = ref<Record<string, string>>({})

// Right-edge-aligned below the trigger, flipped above only when there is no
// room below (WDropdown's place() model, dialog-sized).
function place(): void {
  const btn = wrapEl.value?.querySelector('button')
  const card = panelEl.value
  if (!btn || !card) return
  const r = btn.getBoundingClientRect()
  const vw = window.innerWidth
  const vh = window.innerHeight
  const w = Math.min(400, vw - 16)
  const h = card.offsetHeight || 320
  const left = Math.max(8, Math.min(r.right - w, vw - w - 8))
  const roomBelow = r.bottom + 8 + h <= vh
  const roomAbove = r.top > h + 8
  popStyle.value =
    roomBelow || !roomAbove
      ? {
          left: `${left}px`,
          top: `${Math.max(8, Math.min(r.bottom + 8, vh - h - 8))}px`,
        }
      : { left: `${left}px`, bottom: `${vh - r.top + 8}px` }
}

/* --------------------------- open/close plumbing --------------------------- */

function onDocPointerdown(e: PointerEvent): void {
  const target = e.target as Node
  if (panelEl.value?.contains(target) || wrapEl.value?.contains(target)) return
  // Teleported modal layers sit above the popover and are typically opened
  // from it (the stop/delete confirm dialogs): interacting with one is not
  // an outside click — the panel must survive its own confirm flow.
  if (target instanceof Element && target.closest('.w-modal-backdrop')) return
  sessions.activityPanelOpen = false
}

// Resize (not scroll) closes: a reflow can move or overflow the anchor, and
// re-placing a mid-transition popover flashes. Scroll deliberately does NOT
// close — the capture-phase listener this used to have caught every internal
// scroller (the chat transcript's stick-to-bottom ticks during streaming, the
// panel's own tab body), closing the popover out from under the user. The
// app is a full-viewport layout (html/body/#app height 100%, overflow
// hidden): the document itself never scrolls, so a fixed-position popover
// can never detach from its header anchor, and UIUX §4.1's close set is
// exactly Esc / outside pointerdown / trigger toggle / Tab-out.
function closeExternal(): void {
  sessions.activityPanelOpen = false
}

// Focusables the user can actually reach: inactive tab panels stay in the
// DOM under v-show's inline display:none, and their buttons must not join
// the trap set (a hidden "last" would let Tab escape the popover, and
// focusing a hidden element is a no-op). Checked by walking ancestors for
// display:none/hidden — layout-free, so tests see the same set as browsers.
function visibleFocusables(): HTMLElement[] {
  const root = panelEl.value
  if (!root) return []
  const out: HTMLElement[] = []
  for (const el of root.querySelectorAll<HTMLElement>(
    'button:not([disabled]), [href], input:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )) {
    let n: HTMLElement | null = el
    let hidden = false
    while (n && n !== root) {
      if (n.style.display === 'none' || n.hidden) {
        hidden = true
        break
      }
      n = n.parentElement
    }
    if (!hidden) out.push(el)
  }
  return out
}

function onPanelKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') {
    e.preventDefault()
    close(true)
    return
  }
  // Light focus trap: Tab cycles within the popover's visible controls
  // (UIUX §3 A); Esc (and the outside click) is the way out.
  if (e.key === 'Tab') {
    const f = visibleFocusables()
    if (!f.length) return
    const first = f[0]
    const last = f[f.length - 1]
    const active = document.activeElement
    const inside = !!panelEl.value?.contains(active)
    if (e.shiftKey && (!inside || active === first)) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && (!inside || active === last)) {
      e.preventDefault()
      first.focus()
    }
  }
}

/* ------------------- wake-cap toast (§4.3, panel closed too) ---------------- */

// Rising edge of the ACTIVE session's wake.capped: one amber toast per cap
// EPISODE per session. Mounting into an already-capped session stays
// silent — the toast marks the moment the cap is hit, and the Scheduled tab
// keeps the persistent warning bar afterwards.
const wakeCapped = computed(() =>
  sid.value ? (activity.snapshotOf(sid.value)?.wake.capped ?? false) : false,
)

// Per-session lastToasted guard: sessions that already toasted their current
// cap episode. Switching away and back to a still-capped session must not
// re-toast, so the guard survives session switches; observing the cap
// lifted for a session (falling edge) re-arms it for the next episode.
// Deliberately kept (documented): when the first snapshot this trigger ever
// sees for the ACTIVE session is ALREADY capped, the toast still fires —
// the user was elsewhere when the cap hit and this is how they learn; the
// Scheduled tab's persistent bar only helps once they know to look.
const cappedToasted = new Set<string>()

watch([sid, wakeCapped], ([sidNow, on]) => {
  if (!sidNow) return
  if (!on) {
    // Cap observed lifted: a future episode of this session may toast.
    cappedToasted.delete(sidNow)
    return
  }
  if (cappedToasted.has(sidNow)) return // same episode, already announced
  cappedToasted.add(sidNow)
  toast.warn(
    t('activity.wakeCapped', {
      count: activity.snapshotOf(sidNow)?.wake.pending_notifications ?? 0,
    }),
  )
})

/** "Send a message" (UIUX §4.3): close the popover and hand focus to the
 * composer — ChatPanel owns the composer ref and handles the event. */
function onCompose(): void {
  close(false)
  emit('compose')
}

watch(open, async (on) => {
  if (on) {
    await nextTick()
    place()
    // Focus lands on the first tab (UIUX §4.2: entrance focuses the tablist).
    panelEl.value?.querySelector<HTMLElement>('[role="tab"]')?.focus()
    document.addEventListener('pointerdown', onDocPointerdown)
    window.addEventListener('resize', closeExternal)
  } else {
    document.removeEventListener('pointerdown', onDocPointerdown)
    window.removeEventListener('resize', closeExternal)
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerdown)
  window.removeEventListener('resize', closeExternal)
  window.clearTimeout(pulseTimer)
  if (sessions.activityPanelOpen) sessions.activityPanelOpen = false
})
</script>

<template>
  <span ref="wrapEl" class="act-trigger">
    <WIconButton
      class="act-toggle"
      :class="{ active: open, pulse }"
      :label="triggerLabel"
      :badge="badgeCount"
      :badge-tone="badgeTone"
      :disabled="!sid"
      aria-haspopup="dialog"
      :aria-expanded="open"
      :aria-controls="panelId"
      @click="toggle"
    >
      <ListChecks :size="15" aria-hidden="true" />
    </WIconButton>
    <Teleport to="body">
      <Transition name="panel-pop">
        <div v-if="open && sid" ref="panelEl" class="act-pop" :style="popStyle" @keydown="onPanelKeydown">
          <ActivityPanel :id="panelId" :sid="sid" @close="close(true)" @compose="onCompose" />
        </div>
      </Transition>
    </Teleport>
  </span>
</template>

<style scoped>
.act-trigger {
  display: inline-flex;
  position: relative;
}
.act-toggle.active {
  background: var(--surface-active);
  color: var(--text);
  box-shadow: inset 0 0 0 1px var(--border-strong);
}
/* Badge-count pulse (240ms, one-shot) — covered by the global
 * reduced-motion waiver. */
.act-toggle.pulse {
  animation: act-badge-pulse 240ms var(--ease-in-out);
}
@keyframes act-badge-pulse {
  0%,
  100% {
    background: transparent;
  }
  50% {
    background: var(--brand-soft);
  }
}
</style>

<style>
/* Global (teleported) popover shell: fixed position next to the trigger at
 * the popover z-index; the card inside carries its own scoped styling. */
.act-pop {
  position: fixed;
  z-index: var(--z-popover);
}
</style>
