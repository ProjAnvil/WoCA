<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Brain, Bug, Check, Copy, Target, Wrench } from 'lucide-vue-next'
import type { ChatItem } from '../types'
import { renderMarkdown } from '../lib/markdown'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'
import { useServerSettingsStore } from '../stores/serverSettings'
import Avatar from './Avatar.vue'
import WCollapseCard from './ui/WCollapseCard.vue'

const props = defineProps<{ item: ChatItem }>()
const { t } = useI18n()
const settings = useSettingsStore()
const serverSettings = useServerSettingsStore()
const sessions = useSessionStore()

const isText = computed(() => props.item.kind === 'user' || props.item.kind === 'assistant')
const isUser = computed(() => props.item.kind === 'user')

// Narrow item.kind inside each computed so the discriminated union yields
// the fields directly — no blind casts on the raw item.
const textContent = computed(() => {
  const it = props.item
  return it.kind === 'user' || it.kind === 'assistant' ? it.content : ''
})
const textHtml = computed(() => (isText.value ? renderMarkdown(textContent.value) : ''))
const streaming = computed(() => {
  const it = props.item
  return it.kind === 'assistant' ? it.streaming : false
})
const attachments = computed(() => {
  const it = props.item
  return it.kind === 'user' ? it.attachments : undefined
})

const reasoningContent = computed(() => {
  const it = props.item
  return it.kind === 'reasoning' ? it.content : ''
})
const reasoningStreaming = computed(() => {
  const it = props.item
  return it.kind === 'reasoning' ? it.streaming : false
})
// Live thinking stays expanded so the user can watch it; once the answer
// starts the block collapses to its header (click to expand again).
const reasoningOpen = ref(reasoningStreaming.value)

const goalNoteContent = computed(() => {
  const it = props.item
  return it.kind === 'goal_note' ? it.content : ''
})
const goalNoteStreaming = computed(() => {
  const it = props.item
  return it.kind === 'goal_note' ? it.streaming : false
})
// Goal-loop narration stays open while the loop runs (banners and per-cycle
// chatter are watchable progress), then collapses to its header when the
// unified reply arrives or the run ends.
const goalNoteOpen = ref(goalNoteStreaming.value)
watch(goalNoteStreaming, (live) => {
  if (!live) goalNoteOpen.value = false
})

const toolName = computed(() => {
  const it = props.item
  return it.kind === 'tool_call' ? it.name : ''
})
const toolArgs = computed(() => {
  const it = props.item
  return it.kind === 'tool_call' ? JSON.stringify(it.args, null, 2) : ''
})
// A call without a result is still executing — the header shows a running
// hint instead of the output section.
const toolHasResult = computed(() => {
  const it = props.item
  return it.kind === 'tool_call' && it.result !== undefined
})
const toolResultContent = computed(() => {
  const it = props.item
  return it.kind === 'tool_call' ? (it.result ?? '') : it.kind === 'tool_result' ? it.content : ''
})

// Tool input and output both start collapsed (autoCollapseTools); toggle to
// expand per card.
const collapsed = ref(settings.autoCollapseTools)

/* -------------------------------- copy actions ---------------------------- */

const copied = ref<'md' | 'debug' | ''>('')

function flash(kind: 'md' | 'debug') {
  copied.value = kind
  window.setTimeout(() => {
    copied.value = ''
  }, 1200)
}

function copyText(text: string, kind: 'md' | 'debug') {
  void navigator.clipboard.writeText(text).then(
    () => flash(kind),
    () => flash(kind),
  )
}

// The assistant content is already markdown source — copy it verbatim.
function copyMarkdown() {
  copyText(textContent.value, 'md')
}

// The debug bug copies the log-retrieval context for this bubble: workspace,
// session, run, timestamp, and where the matching log files live.
function copyDebugInfo() {
  // ts / srcRun only exist on the assistant variant — narrow, don't cast.
  const it = props.item
  const ts = it.kind === 'assistant' ? it.ts : undefined
  const srcRun = it.kind === 'assistant' ? it.srcRun : undefined
  const ws = sessions.activeWorkspace
  const sid = sessions.activeId
  const logDir = serverSettings.logDir || '~/.woca/logs'
  const lines = [
    '## woca debug context',
    '',
    `- time: ${ts ? new Date(ts).toISOString() : new Date().toISOString()}`,
    `- workspace: ${ws?.path || '(unknown)'}${ws?.name ? ` (${ws.name})` : ''}`,
    `- session: ${sid || '(unknown)'}`,
    `- run: ${srcRun || sessions.runId?.[sid || ''] || '(ended; see session trace)'}`,
    `- message: content ${textContent.value.length} chars${streaming.value ? ' (still streaming)' : ''}`,
    '',
    'logs (debug-log must be enabled when the message was produced):',
    `- main:    ${logDir}/woca.log`,
    `- session: ${logDir}/ws-<name>-<hash8>/<session>.jsonl`,
    `- message: ${logDir}/ws-<name>-<hash8>/<session>/<run>.json`,
  ]
  copyText(lines.join('\n'), 'debug')
}
</script>

<template>
  <!-- user / assistant text: bubble + avatar (user right, woca left) -->
  <div
    v-if="isText"
    class="message"
    :class="[isUser ? 'user' : 'assistant', { 'message--new': isUser && item.kind === 'user' && item.fresh }]"
  >
    <Avatar :kind="isUser ? 'user' : 'woca'" />
    <div class="message-bubble">
      <div v-if="attachments?.length" class="msg-attachments">
        <img
          v-for="a in attachments"
          :key="a.id"
          :src="a.dataUrl"
          :alt="a.name"
          class="msg-attachment-img"
        />
      </div>
      <div v-if="textContent" class="markdown-body" v-html="textHtml"></div>
      <span v-if="streaming" class="cursor"></span>
      <!-- hover action bar: copy markdown (always) + debug context (when the
           observability log is enabled in settings) -->
      <div v-if="!isUser && !streaming" class="bubble-actions">
        <button
          class="bubble-action"
          :title="t('chat.copyMarkdown')"
          :aria-label="t('chat.copyMarkdown')"
          @click="copyMarkdown()"
        >
          <Check v-if="copied === 'md'" :size="12" />
          <Copy v-else :size="12" />
        </button>
        <button
          v-if="serverSettings.debugLog"
          class="bubble-action debug"
          :title="t('chat.copyDebug')"
          :aria-label="t('chat.copyDebug')"
          @click="copyDebugInfo()"
        >
          <Check v-if="copied === 'debug'" :size="12" />
          <Bug v-else :size="13" />
        </button>
      </div>
    </div>
  </div>

  <!-- reasoning (thinking process): collapsible card, live while streaming -->
  <WCollapseCard class="stream-card"     v-else-if="item.kind === 'reasoning'"
    :title="reasoningStreaming ? t('chat.thinkingLive') : t('chat.thinkingDone')"
    :meta="reasoningStreaming ? '' : t('chat.charCount', { count: reasoningContent.length })"
    tone="brand"
    :live="reasoningStreaming"
    :open="reasoningOpen"
    @update:open="reasoningOpen = $event"
  >
    <template #icon><Brain :size="13" class="reasoning-icon" /></template>
    <pre class="tool-output reasoning-body">{{ reasoningContent }}</pre>
    <span v-if="reasoningStreaming" class="cursor reasoning-cursor"></span>
  </WCollapseCard>

  <!-- goal-loop narration: cycle banners + per-cycle chatter as collapsible
       progress, never an answer bubble (the run ends with one unified reply) -->
  <WCollapseCard class="stream-card"     v-else-if="item.kind === 'goal_note'"
    :title="goalNoteStreaming ? t('chat.goalLoopLive') : t('chat.goalLoopDone')"
    :meta="goalNoteStreaming ? '' : t('chat.charCount', { count: goalNoteContent.length })"
    tone="brand"
    :live="goalNoteStreaming"
    :open="goalNoteOpen"
    @update:open="goalNoteOpen = $event"
  >
    <template #icon><Target :size="13" class="goal-note-icon" /></template>
    <pre class="tool-output reasoning-body">{{ goalNoteContent }}</pre>
    <span v-if="goalNoteStreaming" class="cursor reasoning-cursor"></span>
  </WCollapseCard>

  <!-- tool call: input and output share one card (result merges in when it
       arrives; a still-running call shows a live hint instead of the output) -->
  <WCollapseCard class="stream-card"     v-else-if="item.kind === 'tool_call'"
    :title="toolName"
    :meta="toolHasResult ? '' : t('chat.running')"
    :live="!toolHasResult"
    :open="!collapsed"
    @update:open="collapsed = !$event"
  >
    <template #icon><Wrench :size="13" /></template>
    <template v-if="!collapsed">
      <div class="tool-section-label">{{ t('chat.input') }}</div>
      <pre class="tool-args tool-io">{{ toolArgs }}</pre>
      <template v-if="toolHasResult">
        <div class="tool-section-label">{{ t('chat.output') }}</div>
        <pre class="tool-output tool-io">{{ toolResultContent }}</pre>
      </template>
    </template>
  </WCollapseCard>

  <!-- tool result (orphan: its call is no longer in the transcript) -->
  <WCollapseCard class="stream-card" v-else :title="t('chat.toolOutput')" :open="!collapsed" @update:open="collapsed = !$event">
    <template v-if="!collapsed">
      <pre class="tool-output">{{ toolResultContent }}</pre>
    </template>
  </WCollapseCard>
</template>
