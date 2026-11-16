<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Copy } from 'lucide-vue-next'
import { useToastStore } from '../../stores/toast'
import type { SubagentRun } from '../../types'

// The readonly transcript surface of one subagent child (§4.2): mono,
// max-h 200px scroll, copy button, role="log". V1 source decision (Task
// 12): the registry's Report field — the child's live transcript stays in
// its checkpoint namespace and nothing streams it to the web, so a running
// child shows the live placeholder and the finished child its final
// report. No new transcript storage was built.
const props = defineProps<{ run: SubagentRun }>()

const { t } = useI18n()
const toast = useToastStore()

const text = computed(() => {
  const report = props.run.report?.trim()
  if (report) return report
  // Done/stopped/failed without a report is NOT "still running" — render
  // the terminal state honestly instead of a misleading live label.
  if (props.run.state === 'done' || props.run.state === 'failed' || props.run.state === 'stopped') {
    return t('activity.subagentsEmptyReport')
  }
  return t('activity.transcriptLive')
})

async function copy(): Promise<void> {
  try {
    await navigator.clipboard.writeText(text.value)
    toast.success(t('common.copied'))
  } catch {
    toast.error(t('common.error'))
  }
}
</script>

<template>
  <div class="sub-transcript">
    <div class="sub-transcript-bar">
      <span class="sub-transcript-note">{{ run.def }} · {{ t(`activity.subStatus.${run.state}`) }}</span>
      <button
        class="act-link-btn"
        type="button"
        data-testid="sub-transcript-copy"
        :title="t('activity.outputCopy')"
        @click="copy"
      >
        <Copy :size="12" aria-hidden="true" />
        {{ t('common.copy') }}
      </button>
    </div>
    <pre
      class="sub-transcript-text mono capped"
      :data-testid="`sub-transcript-${run.call_id}`"
      role="log"
      :aria-label="t('activity.viewTranscript')"
    >{{ text }}</pre>
  </div>
</template>

<style scoped>
.sub-transcript {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  margin: var(--space-2) 0 0;
  padding-left: var(--space-2);
  border-left: 2px solid var(--border);
}
.sub-transcript-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.sub-transcript-note {
  font-size: var(--text-xs);
  color: var(--muted);
}
/* The copy affordance shares the OutputTail quiet-ghost link button
 * (.act-link-btn lives in OutputTail.vue's scoped styles — redeclared
 * here for this scope). */
.act-link-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: none;
  padding: 2px 4px;
  background: transparent;
  color: var(--muted);
  font: inherit;
  font-size: var(--text-xs);
  cursor: pointer;
  border-radius: var(--radius-xs);
}
.act-link-btn:hover {
  color: var(--text);
  background: var(--surface-hover);
}
.act-link-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.sub-transcript-text {
  margin: 0;
  max-height: 200px;
  overflow-y: auto;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--text);
  white-space: pre-wrap;
  word-break: break-word;
}
.sub-transcript-text.mono {
  font-family: var(--font-mono);
}
</style>
