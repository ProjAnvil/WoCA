<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Bot, ChevronDown, ChevronRight, OctagonX, Terminal } from 'lucide-vue-next'
import { useActivityStore, type OutputKind } from '../../stores/activity'
import { useConfirmStore } from '../../stores/confirm'
import { fmtDuration } from '../../lib/format'
import OutputTail from './OutputTail.vue'
import type { BackgroundShell, BackgroundTask, ItemStatus, StoppedBy } from '../../types'

// Background shells + subagent tasks as one merged list (UIUX §2): running
// items pinned to the top (longest first), terminal items newest-end first.
// Stopping goes through WConfirmDialog; the D1 wording tells the user the
// agent learns about the user stop with their next message.
const props = defineProps<{ sid: string }>()

const { t } = useI18n()
const activity = useActivityStore()
const confirm = useConfirmStore()

interface Row {
  key: string
  kind: OutputKind
  id: string
  name: string
  status: ItemStatus
  exitCode: number | null
  stoppedBy: StoppedBy
  startedAt: number
  endedAt: number | null
}

function shellRow(x: BackgroundShell): Row {
  return {
    key: `shell:${x.id}`,
    kind: 'shell',
    id: x.id,
    name: x.command,
    status: x.status,
    exitCode: x.exit_code,
    stoppedBy: x.stopped_by,
    startedAt: Date.parse(x.started_at),
    endedAt: x.ended_at ? Date.parse(x.ended_at) : null,
  }
}

function taskRow(x: BackgroundTask): Row {
  return {
    key: `task:${x.id}`,
    kind: 'task',
    id: x.id,
    name: x.description,
    status: x.status,
    exitCode: null,
    stoppedBy: x.stopped_by,
    startedAt: Date.parse(x.started_at),
    endedAt: x.ended_at ? Date.parse(x.ended_at) : null,
  }
}

const rows = computed<Row[]>(() => {
  const s = activity.snapshotOf(props.sid)
  if (!s) return []
  const all = [...s.shells.map(shellRow), ...s.tasks.map(taskRow)]
  return all.sort((a, b) => {
    const aRunning = a.status === 'running' ? 0 : 1
    const bRunning = b.status === 'running' ? 0 : 1
    if (aRunning !== bRunning) return aRunning - bRunning
    if (aRunning === 0) return a.startedAt - b.startedAt // longest-running first
    return (b.endedAt ?? 0) - (a.endedAt ?? 0) // newest finish first
  })
})

function statusLabel(r: Row): string {
  switch (r.status) {
    case 'running':
      return t('activity.status.running')
    case 'completed':
      return t('activity.status.completed')
    case 'failed':
      return r.exitCode != null
        ? t('activity.status.failedExit', { code: r.exitCode })
        : t('activity.status.failed')
    case 'stopped':
      if (r.stoppedBy === 'user') return t('activity.status.stoppedByUser')
      if (r.stoppedBy === 'model') return t('activity.status.stoppedByModel')
      return t('activity.status.stopped')
  }
}

// Rows with a stop request in flight (UIUX §4.2 "stopping"): their stop
// button disables until the post-action refetch lands.
const stopping = ref<ReadonlySet<string>>(new Set())

async function stop(r: Row): Promise<void> {
  const ok = await confirm.ask({
    title: t('activity.confirmStopTitle'),
    body: t('activity.confirmStopBody', { name: r.name }),
    confirmLabel: t('activity.confirmStopAction'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  stopping.value = new Set(stopping.value).add(r.key)
  try {
    // The store awaits the REST call then refetches server truth; a failure
    // lands in this item's inline error slot.
    if (r.kind === 'shell') await activity.killShell(props.sid, r.id)
    else await activity.stopTask(props.sid, r.id)
  } finally {
    const next = new Set(stopping.value)
    next.delete(r.key)
    stopping.value = next
  }
}

/* ----------------------- running clocks (1s tick) ------------------------- */

// Remote-clock hint: elapsed math runs on the server's timeline so a relay
// lag does not skew "running for" (snapshot.server_time, PM §4.3).
const clockOffsetMs = computed(() => {
  const s = activity.snapshotOf(props.sid)
  if (!s) return 0
  const parsed = Date.parse(s.server_time)
  return Number.isNaN(parsed) ? 0 : parsed - Date.now()
})

const now = ref(Date.now())
let tick: number | undefined

onMounted(() => {
  tick = window.setInterval(() => (now.value = Date.now()), 1000)
})
onBeforeUnmount(() => window.clearInterval(tick))

function duration(r: Row): string {
  if (r.status === 'running') return fmtDuration(now.value + clockOffsetMs.value - r.startedAt)
  if (r.endedAt != null) return fmtDuration(r.endedAt - r.startedAt)
  return ''
}

/* --------------------------- output expansion ----------------------------- */

const expanded = ref<ReadonlySet<string>>(new Set())

function toggleOutput(r: Row): void {
  const next = new Set(expanded.value)
  if (next.has(r.key)) next.delete(r.key)
  else next.add(r.key)
  expanded.value = next
}

/* ------------------- completion announcements (§4.5) ---------------------- */

// aria-live: a running item reaching a terminal status is announced once
// (polite); the initial mount only seeds the baseline.
const announcement = ref('')
const prevStatuses = new Map<string, ItemStatus>()

watch(
  rows,
  (list) => {
    for (const r of list) {
      const was = prevStatuses.get(r.key)
      if (was === 'running') {
        if (r.status === 'completed') {
          announcement.value = t('activity.liveFinished', { name: r.name })
        } else if (r.status === 'failed') {
          announcement.value = t('activity.liveFailed', { name: r.name, code: r.exitCode ?? 1 })
        }
      }
      prevStatuses.set(r.key, r.status)
    }
  },
  { immediate: true },
)

/* ----------------------------- row keyboard ------------------------------- */

// ↑/↓ rove between rows (Tab still reaches the inner buttons); Enter on a
// focused row expands its output (UIUX §4.5 keyboard path).
const listEl = ref<HTMLElement | null>(null)

function onListKeydown(e: KeyboardEvent): void {
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter') return
  const rowEls = Array.from(listEl.value?.querySelectorAll<HTMLElement>('[data-row-key]') ?? [])
  if (!rowEls.length) return
  const idx = rowEls.indexOf(document.activeElement as HTMLElement)
  if (e.key === 'Enter') {
    if (idx < 0) return // focus is on an inner control: native Enter applies
    e.preventDefault()
    const r = rows.value.find((x) => x.key === rowEls[idx].dataset.rowKey)
    if (r) toggleOutput(r)
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
    <div v-if="rows.length" ref="listEl" class="act-bg-list" @keydown="onListKeydown">
      <p class="sr-only" aria-live="polite" role="status">{{ announcement }}</p>
      <div
        v-for="r in rows"
        :key="r.key"
        class="act-row"
        :class="`st-${r.status}`"
        :data-row-key="r.key"
        tabindex="-1"
      >
        <div class="act-row-main">
          <component
            :is="r.kind === 'shell' ? Terminal : Bot"
            :size="13"
            class="act-row-kind"
            aria-hidden="true"
          />
          <span class="sr-only">
            {{ (r.kind === 'shell' ? t('activity.kindShell') : t('activity.kindTask')) + ': ' }}
          </span>
          <span class="act-row-name" :title="r.name">{{ r.name }}</span>
          <span class="act-row-status" :class="`st-${r.status}`">
            <span v-if="r.status === 'running'" class="act-breathe" aria-hidden="true" />
            {{ statusLabel(r) }}
          </span>
          <span v-if="duration(r)" class="act-row-dur">{{ duration(r) }}</span>
          <button
            v-if="r.status === 'running'"
            class="act-stop-btn"
            type="button"
            :title="t('activity.stop')"
            :disabled="stopping.has(r.key)"
            @click="stop(r)"
          >
            <OctagonX :size="13" aria-hidden="true" />
            <span>{{ t('activity.stop') }}</span>
          </button>
          <button
            class="act-out-toggle"
            type="button"
            :aria-expanded="expanded.has(r.key)"
            :aria-label="expanded.has(r.key) ? t('activity.outputHide') : t('activity.outputShow')"
            @click="toggleOutput(r)"
          >
            <ChevronDown v-if="expanded.has(r.key)" :size="13" aria-hidden="true" />
            <ChevronRight v-else :size="13" aria-hidden="true" />
          </button>
        </div>
        <p
          v-if="activity.actionErrorOf(props.sid, r.kind, r.id)"
          class="act-row-err"
          role="alert"
        >
          {{ t('activity.actionFailed', { error: activity.actionErrorOf(props.sid, r.kind, r.id) }) }}
        </p>
        <OutputTail v-if="expanded.has(r.key)" :sid="props.sid" :kind="r.kind" :id="r.id" />
      </div>
    </div>
    <div v-else class="act-empty">
      <Terminal :size="16" aria-hidden="true" />
      <span>{{ t('activity.backgroundEmpty') }}</span>
    </div>
  </div>
</template>

<style scoped>
.act-bg-list {
  padding: var(--space-2) var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.act-row {
  display: flex;
  flex-direction: column;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-left: 2px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--surface);
}
.act-row:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
/* The left rail carries the state color (shape + text carry it too). */
.act-row.st-running {
  border-left-color: var(--brand);
}
.act-row.st-completed {
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
.act-row-kind {
  flex-shrink: 0;
  color: var(--muted);
}
.act-row-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--text);
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
.act-row-status.st-completed {
  color: var(--ok-text);
}
.act-row-status.st-failed {
  color: var(--danger-text);
}
.act-row-status.st-stopped {
  color: var(--muted);
}
.act-breathe {
  width: 7px;
  height: 7px;
  border-radius: var(--radius-full);
  background: var(--brand);
  animation: task-pill-breathe 1.6s ease-in-out infinite;
}
.act-row-dur {
  flex-shrink: 0;
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
/* The stop button's danger-ghost styling is shared with ScheduledTab's
 * delete — see .act-stop-btn in styles/panels.css (global, not scoped). */
.act-out-toggle {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
}
.act-out-toggle:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.act-out-toggle:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.act-row-err {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--danger);
  overflow-wrap: anywhere;
}
</style>
