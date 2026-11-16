<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronDown, ChevronRight, ArrowRight } from 'lucide-vue-next'
import { fmtDuration } from '../../lib/format'
import type { SubagentRun } from '../../types'

// One child row of the Subagents tab (§4.2): status dot + text (dual
// channel — running=brand breathing, waiting=warn, done=ok, failed=danger,
// stopped=muted), def name, elapsed, tokens (tabular-nums). Click toggles
// the in-panel transcript reveal; a waiting row embeds the go-to-approval
// affordance (§4.1) handled by the parent tab.
const props = defineProps<{
  run: SubagentRun
  expanded: boolean
  /** Server-tick clock for elapsed math (shared interval, one per tab). */
  now: number
  /** Remote-clock hint (snapshot.server_time − local), ms. */
  clockOffsetMs: number
}>()

const emit = defineEmits<{ toggle: []; goto: [] }>()

const { t } = useI18n()

const state = computed(() => props.run.state)
const statusLabel = computed(() => t(`activity.subStatus.${state.value}`))

const startedAt = computed(() => Date.parse(props.run.started_at))
const endedAt = computed(() => (props.run.ended_at ? Date.parse(props.run.ended_at) : null))
const duration = computed(() => {
  if (endedAt.value != null) return fmtDuration(endedAt.value - startedAt.value)
  return fmtDuration(props.now + props.clockOffsetMs - startedAt.value)
})
</script>

<template>
  <div
    class="act-row sub-row"
    :class="`st-${state}`"
    :data-testid="`sub-row-${run.call_id}`"
    :data-row-key="run.call_id"
    :title="run.origin"
    :aria-expanded="expanded"
    tabindex="-1"
    @click="emit('toggle')"
  >
    <div class="act-row-main">
      <span class="act-row-name">{{ run.def }}<span class="sub-row-short">#{{ run.call_id.slice(-4) }}</span></span>
      <span
        class="act-row-status"
        :class="`st-${state}`"
        :data-testid="`sub-status-${run.call_id}`"
      >
        <span v-if="state === 'running'" class="act-breathe" aria-hidden="true" />
        <span v-else class="sub-dot" aria-hidden="true" />
        {{ statusLabel }}
      </span>
      <span v-if="run.tokens" class="sub-row-tokens tabular" :title="t('activity.subagents')">
        {{ run.tokens.toLocaleString() }}
      </span>
      <span class="act-row-dur">{{ duration }}</span>
      <button
        v-if="state === 'waiting'"
        class="sub-goto-btn"
        type="button"
        :data-testid="`sub-goto-approval-${run.call_id}`"
        :aria-label="t('activity.subGotoApprovalLabel', { name: run.def })"
        @click.stop="emit('goto')"
      >
        {{ t('activity.subGotoApproval') }}
        <ArrowRight :size="12" aria-hidden="true" />
      </button>
      <span class="sub-caret" aria-hidden="true">
        <ChevronDown v-if="expanded" :size="13" />
        <ChevronRight v-else :size="13" />
      </span>
    </div>
  </div>
</template>

<style scoped>
/* Row chrome (card, rail, focus ring, name/status/dur columns) reuses the
 * BackgroundTab row grammar — see the shared classes in this file's parent
 * (styles/panels.css carries .act-stop-btn; the row block below mirrors
 * BackgroundTab's scoped rules so both tabs stay visually one family). */
.act-row {
  display: flex;
  flex-direction: column;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-left: 2px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
  cursor: pointer;
}
.act-row:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.act-row.st-running {
  border-left-color: var(--brand);
}
.act-row.st-waiting {
  border-left-color: var(--warn);
}
.act-row.st-done {
  border-left-color: var(--ok);
}
.act-row.st-failed {
  border-left-color: var(--danger);
}
.act-row.st-stopped {
  border-left-color: var(--muted);
}
.act-row-main {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  min-width: 0;
}
.act-row-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-xs);
  color: var(--text);
}
.sub-row-short {
  font-family: var(--font-mono);
  color: var(--muted);
}
.act-row-status {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  font-variant-numeric: tabular-nums;
}
.act-row-status.st-running {
  color: var(--brand-text);
}
.act-row-status.st-waiting {
  color: var(--warn-text);
}
.act-row-status.st-done {
  color: var(--ok-text);
}
.act-row-status.st-failed {
  color: var(--danger-text);
}
.act-row-status.st-stopped {
  color: var(--muted);
}
/* The waiting dot is a plain warn pip: waiting is a resting state (the
 * breathing is reserved for active execution). */
.sub-dot {
  width: 7px;
  height: 7px;
  border-radius: var(--radius-full);
  background: currentColor;
}
.act-breathe {
  width: 7px;
  height: 7px;
  border-radius: var(--radius-full);
  background: var(--brand);
  animation: task-pill-breathe 1.6s ease-in-out infinite;
}
.sub-row-tokens {
  flex-shrink: 0;
  font-size: var(--text-xs);
  color: var(--muted);
}
.sub-row-tokens.tabular,
.act-row-dur {
  font-variant-numeric: tabular-nums;
}
.act-row-dur {
  flex-shrink: 0;
  font-size: var(--text-xs);
  color: var(--muted);
}
.sub-goto-btn {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 3px 8px;
  border: 1px solid var(--warn);
  border-radius: var(--radius-full);
  background: var(--warn-soft);
  color: var(--warn-text);
  font: inherit;
  font-size: var(--text-xs);
  cursor: pointer;
}
.sub-goto-btn:hover {
  color: var(--text);
}
.sub-goto-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
/* ≥44px touch targets under 640px (panels.css precedent). */
@media (max-width: 640px) {
  .sub-goto-btn {
    min-height: 44px;
    padding: 6px 12px;
  }
}
.sub-caret {
  flex-shrink: 0;
  display: inline-flex;
  color: var(--muted);
}
</style>
