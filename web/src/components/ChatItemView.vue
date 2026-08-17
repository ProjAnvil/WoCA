<script setup lang="ts">
import { computed, ref } from 'vue'
import { Brain, Bug, Check, ChevronRight, Copy, Wrench } from 'lucide-vue-next'
import type { ChatItem } from '../types'
import { renderMarkdown } from '../lib/markdown'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'
import Avatar from './Avatar.vue'

const props = defineProps<{ item: ChatItem }>()
const settings = useSettingsStore()
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
  const logDir = settings.logDir || '~/.woca/logs'
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
  <div v-if="isText" class="message" :class="isUser ? 'user' : 'assistant'">
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
          title="复制为 Markdown"
          @click="copyMarkdown()"
        >
          <Check v-if="copied === 'md'" :size="12" />
          <Copy v-else :size="12" />
        </button>
        <button
          v-if="settings.debugLog"
          class="bubble-action debug"
          title="复制调试信息（workspace / session / message 与日志位置）"
          @click="copyDebugInfo()"
        >
          <Check v-if="copied === 'debug'" :size="12" />
          <Bug v-else :size="13" />
        </button>
      </div>
    </div>
  </div>

  <!-- reasoning (thinking process): collapsible block, live while streaming -->
  <div v-else-if="item.kind === 'reasoning'" class="tool-card reasoning-card" :class="{ live: reasoningStreaming }">
    <button class="tool-card-head tool-toggle" @click="reasoningOpen = !reasoningOpen">
      <Brain :size="13" class="reasoning-icon" />
      <ChevronRight :size="13" class="tool-chevron" :class="{ open: reasoningOpen }" />
      <span class="tool-name">{{ reasoningStreaming ? '思考中…' : '思考过程' }}</span>
      <span v-if="!reasoningStreaming" class="reasoning-size">{{ reasoningContent.length }} 字符</span>
    </button>
    <pre v-if="reasoningOpen" class="tool-output reasoning-body">{{ reasoningContent }}</pre>
    <span v-else-if="reasoningStreaming" class="cursor reasoning-cursor"></span>
  </div>

  <!-- tool call: input and output share one card (result merges in when it
       arrives; a still-running call shows a hint instead of the output) -->
  <div v-else-if="item.kind === 'tool_call'" class="tool-card">
    <button class="tool-card-head tool-toggle" @click="collapsed = !collapsed">
      <Wrench :size="13" />
      <ChevronRight :size="13" class="tool-chevron" :class="{ open: !collapsed }" />
      <span class="tool-name">{{ toolName }}</span>
      <span v-if="!toolHasResult" class="tool-status">运行中…</span>
    </button>
    <template v-if="!collapsed">
      <div class="tool-section-label">输入</div>
      <pre class="tool-args tool-io">{{ toolArgs }}</pre>
      <template v-if="toolHasResult">
        <div class="tool-section-label">输出</div>
        <pre class="tool-output tool-io">{{ toolResultContent }}</pre>
      </template>
    </template>
  </div>

  <!-- tool result (orphan: its call is no longer in the transcript) -->
  <div v-else class="tool-card tool-result">
    <button class="tool-card-head tool-toggle" @click="collapsed = !collapsed">
      <ChevronRight :size="13" class="tool-chevron" :class="{ open: !collapsed }" />
      <span class="tool-name">工具输出</span>
    </button>
    <pre v-if="!collapsed" class="tool-output">{{ toolResultContent }}</pre>
  </div>
</template>
