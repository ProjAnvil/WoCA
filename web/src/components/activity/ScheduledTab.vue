<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { CalendarClock, Trash2 } from 'lucide-vue-next'
import { useActivityStore } from '../../stores/activity'
import { useConfirmStore } from '../../stores/confirm'
import { fmtDateTime } from '../../lib/format'
import type { CronEntry } from '../../types'

// Scheduled prompts (cron): future-facing entries with a destructive delete
// behind WConfirmDialog (cancelling future automatic runs deserves a second
// look). The wake status lines give capped/stalled/disabled/pending a
// readable explanation (PM §4.9).
const props = defineProps<{ sid: string }>()

const { t } = useI18n()
const activity = useActivityStore()
const confirm = useConfirmStore()

const crons = computed(() => activity.snapshotOf(props.sid)?.crons ?? [])
const wake = computed(() => activity.snapshotOf(props.sid)?.wake ?? null)

async function remove(entry: CronEntry): Promise<void> {
  const ok = await confirm.ask({
    title: t('activity.confirmDeleteTitle'),
    body: t('activity.confirmDeleteBody', { name: entry.name, spec: entry.spec }),
    confirmLabel: t('activity.confirmDeleteAction'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  // The store refetches after the action; failures land in the item's
  // inline error slot.
  await activity.deleteCron(props.sid, entry.id)
}
</script>

<template>
  <div class="act-tab-body">
    <p v-if="wake && wake.capped" class="act-wake warn" role="status">
      {{ t('activity.wakeCapped', { count: wake.pending_notifications }) }}
    </p>
    <p v-if="wake && wake.stalled" class="act-wake warn" role="status">
      {{ t('activity.wakeStalled') }}
    </p>
    <p
      v-else-if="wake && wake.pending_notifications > 0 && !wake.capped"
      class="act-wake"
      role="status"
    >
      {{ t('activity.wakePending', { count: wake.pending_notifications }) }}
    </p>
    <p v-if="wake && !wake.enabled" class="act-wake off">
      {{ t('activity.wakeDisabled') }}
    </p>

    <ul v-if="crons.length" class="act-cron-list">
      <li v-for="c in crons" :key="c.id" class="act-cron-row">
        <div class="act-cron-main">
          <CalendarClock :size="14" class="act-cron-icon" aria-hidden="true" />
          <span class="act-cron-name" :title="c.name">{{ c.name }}</span>
          <code class="act-cron-spec">{{ c.spec }}</code>
          <button
            class="act-stop-btn"
            type="button"
            :title="t('activity.delete')"
            :aria-label="t('activity.delete')"
            @click="remove(c)"
          >
            <Trash2 :size="13" aria-hidden="true" />
          </button>
        </div>
        <dl class="act-cron-meta">
          <div class="act-cron-meta-row">
            <dt>{{ c.next_fire_at ? t('activity.cronNext', { time: fmtDateTime(c.next_fire_at) }) : t('activity.cronNoNext') }}</dt>
            <dd>
              {{
                c.last_fired_at
                  ? t('activity.cronLast', { time: fmtDateTime(c.last_fired_at) })
                  : t('activity.cronNever')
              }}
            </dd>
          </div>
          <div class="act-cron-meta-row">
            <dt class="act-cron-prompt-label">{{ t('activity.cronPrompt') }}</dt>
            <dd class="act-cron-prompt" :title="c.prompt">{{ c.prompt }}</dd>
          </div>
        </dl>
        <p
          v-if="activity.actionErrorOf(props.sid, 'cron', c.id)"
          class="act-row-err"
          role="alert"
        >
          {{ t('activity.actionFailed', { error: activity.actionErrorOf(props.sid, 'cron', c.id) }) }}
        </p>
      </li>
    </ul>
    <div v-else class="act-empty">
      <CalendarClock :size="16" aria-hidden="true" />
      <span>{{ t('activity.scheduledEmpty') }}</span>
    </div>
  </div>
</template>

<style scoped>
.act-wake {
  margin: 0 var(--space-4) var(--space-3);
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--text-2);
}
.act-wake.warn {
  border-color: color-mix(in srgb, var(--warn) 45%, var(--border));
  background: var(--warn-soft);
  color: var(--warn-text);
}
.act-wake.off {
  color: var(--muted);
}
.act-cron-list {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.act-cron-row {
  padding: var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
}
.act-cron-main {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  min-width: 0;
}
.act-cron-icon {
  flex-shrink: 0;
  color: var(--muted);
}
.act-cron-name {
  font-weight: 600;
  font-size: var(--text-sm);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.act-cron-spec {
  flex-shrink: 0;
  padding: 0 var(--space-2);
  border-radius: var(--radius-xs);
  background: var(--surface-2);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--text-2);
}
.act-cron-meta {
  margin: var(--space-2) 0 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.act-cron-meta-row {
  display: flex;
  align-items: baseline;
  gap: var(--space-3);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
}
.act-cron-meta-row dt {
  color: var(--muted);
  white-space: nowrap;
}
.act-cron-meta-row dd {
  margin: 0;
  color: var(--text-2);
  min-width: 0;
}
.act-cron-prompt {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.act-cron-prompt-label {
  flex-shrink: 0;
}
.act-stop-btn {
  margin-left: auto;
}
.act-row-err {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--danger);
  overflow-wrap: anywhere;
}
</style>
