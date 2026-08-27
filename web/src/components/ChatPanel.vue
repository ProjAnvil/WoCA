<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowDown, Brain, Check, PanelRight, Target } from 'lucide-vue-next'
import ChatItemView from './ChatItemView.vue'
import Composer from './composer/Composer.vue'
import WEmptyState from './ui/WEmptyState.vue'
import WSkeleton from './ui/WSkeleton.vue'
import { useSessionStore } from '../stores/sessions'
import { useWorkspaceStore } from '../stores/workspace'
import { useMemoryStore } from '../stores/memory'
import { useStickToBottom } from '../composables/useStickToBottom'
import { toolEventSummary } from '../lib/toolEvent'

// Chat surface: header (session identity + panel toggles), the message
// stream (stick-to-bottom + skeletons + empty state), the streaming
// liveness strip, and the composer. Everything input-shaped lives in
// components/composer/*.
const { t } = useI18n()
const store = useSessionStore()
const workspace = useWorkspaceStore()
const memory = useMemoryStore()
const workspaceStore = useWorkspaceStore()

const busy = computed(() => ['running', 'awaiting', 'queued'].includes(store.activeStatus))

/* ----------------------- message stream: stick to bottom ------------------ */

// Streaming output keeps the view pinned to the bottom while the user is
// there; scrolling up detaches (no yank while reading) and surfaces a
// "jump to latest" pill counting what they missed.
const messagesEl = ref<HTMLElement | null>(null)
const { pinned, missed, follow, jumpToLatest } = useStickToBottom(messagesEl)

// The computed re-evaluates on every transcript mutation (new items and
// in-place streaming growth alike) — exactly the moments to follow.
watch(() => store.activeItems, follow)

// Switching sessions lands at the newest messages, re-pinned.
watch(
  () => store.activeId,
  () => jumpToLatest(),
)

/* ------------------------------ empty state ------------------------------- */

// One-click sample prompts: a fresh session starts with something to run,
// not a blank page (industry baseline since Jules' one-click prompts).
const composerEl = ref<InstanceType<typeof Composer> | null>(null)
const emptyPrompts = computed(() => [t('chat.emptyPrompt1'), t('chat.emptyPrompt2'), t('chat.emptyPrompt3')])

function usePrompt(p: string) {
  composerEl.value?.fillPrompt(p)
}

/* --------------------------- streaming liveness ---------------------------- */

// While a run is active the composer edge shows a liveness strip: a
// breathing dot, the elapsed time, and the latest tool event — "still
// alive, and here is what it is doing right now". The clock starts when
// this tab observes the run (attach-time for runs resumed mid-flight).
const runStartedAt = ref<number | null>(null)
const elapsedLabel = ref('')
let livenessTimer: number | undefined

function fmtElapsed(start: number): string {
  const sec = Math.max(0, Math.floor((Date.now() - start) / 1000))
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

function tickElapsed() {
  if (runStartedAt.value !== null) elapsedLabel.value = fmtElapsed(runStartedAt.value)
}

const running = computed(() => store.activeStatus === 'running')

watch(
  running,
  (on) => {
    window.clearInterval(livenessTimer)
    if (on) {
      if (runStartedAt.value === null) runStartedAt.value = Date.now()
      tickElapsed()
      livenessTimer = window.setInterval(tickElapsed, 1000)
    } else {
      runStartedAt.value = null
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => window.clearInterval(livenessTimer))

// When a run ends, pull the pending-change count so the done capsule can
// show "N file changes" and the Changes tab badge stays fresh.
watch(running, (on) => {
  if (!on) void workspaceStore.refreshChanges()
})

// Newest tool event wins; text replies stream via the assistant bubble.
const latestEvent = computed(() => {
  const items = store.activeItems
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind === 'tool_call') return toolEventSummary(it.name, it.args)
  }
  return ''
})
</script>

<template>
  <div class="chat">
    <header class="chat-header">
      <div class="chat-title-wrap">
        <span class="chat-title">{{ store.activeSession?.title ?? 'woca' }}</span>
        <span v-if="store.activeStatus === 'queued'" class="task-pill pill-queued">
          <span class="pill-dot" aria-hidden="true" />
          {{ t('chat.queued', { position: store.queuePosition[store.activeId] ?? 1 }) }}
        </span>
        <span v-else-if="running" class="task-pill pill-running" role="status">
          <span class="pill-dot" aria-hidden="true" />
          {{ t('chat.runningFor', { time: elapsedLabel }) }}
        </span>
        <span v-else-if="store.activeStatus === 'awaiting'" class="task-pill pill-awaiting">
          <span class="pill-dot" aria-hidden="true" />
          {{ t('chat.awaitingConfirm') }}
        </span>
        <button
          v-else-if="store.activeStatus === 'done' && workspaceStore.changes.length"
          class="task-pill pill-done"
          type="button"
          :title="t('chat.doneChangesTitle')"
          @click="workspaceStore.openChangesTab()"
        >
          <Check :size="12" aria-hidden="true" />
          {{ t('chat.doneChanges', { count: workspaceStore.changes.length }) }}
        </button>
        <button
          v-if="store.activeGoal"
          class="goal-chip"
          :class="`s-${store.activeGoal.status}`"
          :title="t('chat.goalChipTitle')"
          @click="store.goalPanelOpen = true"
        >
          <Target :size="13" />
          <span class="goal-chip-text">{{ store.activeGoal.objective }}</span>
          <span class="goal-chip-meta">C{{ store.activeGoal.cycles }}/{{ store.activeGoal.max_cycles }}</span>
        </button>
      </div>
      <button
        class="icon-btn mem-toggle"
        :title="t('chat.memoryTitle')"
        :aria-label="t('chat.memoryTitle')"
        @click="memory.open(store.activeWorkspace?.path ?? '', store.activeWorkspace?.name)"
      >
        <Brain :size="15" />
        <span class="mem-toggle-label">{{ t('chat.memory') }}</span>
      </button>
      <button
        class="icon-btn panel-toggle"
        :class="{ active: workspace.panelOpen }"
        :title="workspace.panelOpen ? t('chat.collapseWorkspace') : t('chat.expandWorkspace')"
        :aria-label="workspace.panelOpen ? t('chat.collapseWorkspace') : t('chat.expandWorkspace')"
        :aria-pressed="workspace.panelOpen"
        @click="workspace.togglePanel()"
      >
        <PanelRight :size="17" />
      </button>
    </header>

    <div class="chat-stream">
      <div ref="messagesEl" class="chat-messages">
        <ChatItemView v-for="item in store.activeItems" :key="item.id" :item="item" />
        <div v-if="store.historyLoading" class="msg-skeleton-wrap" role="status" aria-busy="true">
          <span class="sr-only">{{ t('common.loading') }}</span>
          <div v-for="i in 3" :key="i" class="msg-skeleton">
            <WSkeleton variant="circle" width="28px" height="28px" />
            <div class="msg-skeleton-body">
              <WSkeleton variant="text" width="62%" />
              <WSkeleton variant="text" width="94%" />
              <WSkeleton variant="text" width="78%" />
            </div>
          </div>
        </div>
        <div v-if="store.activeStatus === 'error'" class="error-banner">
          {{ store.activeError }}
        </div>
        <WEmptyState v-if="store.activeItems.length === 0 && !busy && !store.historyLoading" :hint="t('chat.emptyHint')">
          <template #actions>
            <button v-for="p in emptyPrompts" :key="p" class="empty-prompt-chip" type="button" @click="usePrompt(p)">
              {{ p }}
            </button>
          </template>
        </WEmptyState>
      </div>
      <Transition name="jump-latest">
        <button v-if="!pinned" class="jump-latest" :aria-label="t('chat.jumpToLatest')" @click="jumpToLatest()">
          <ArrowDown :size="14" />
          <span v-if="missed" class="jump-latest-count">{{ missed > 99 ? '99+' : missed }}</span>
          <span class="jump-latest-label">{{ t('chat.jumpToLatest') }}</span>
        </button>
      </Transition>
    </div>

    <div v-if="running" class="stream-liveness" role="status">
      <span class="liveness-dot" aria-hidden="true" />
      <span class="liveness-elapsed">{{ t('chat.runningFor', { time: elapsedLabel }) }}</span>
      <span v-if="latestEvent" class="liveness-event" :title="latestEvent">{{ latestEvent }}</span>
    </div>

    <Composer ref="composerEl" />
  </div>
</template>
