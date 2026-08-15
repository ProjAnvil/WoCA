<script setup lang="ts">
import { computed, ref } from 'vue'
import { Bug, Check, ChevronRight, Copy, Wrench } from 'lucide-vue-next'
import type { Attachment, ChatItem } from '../types'
import { renderMarkdown } from '../lib/markdown'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'
import Avatar from './Avatar.vue'

const props = defineProps<{ item: ChatItem }>()
const settings = useSettingsStore()
const sessions = useSessionStore()

const isText = computed(() => props.item.kind === 'user' || props.item.kind === 'assistant')
const isUser = computed(() => props.item.kind === 'user')

const textContent = computed(() =>
  isText.value ? (props.item as { content: string }).content : '',
)
const textHtml = computed(() => (isText.value ? renderMarkdown(textContent.value) : ''))
const streaming = computed(() =>
  props.item.kind === 'assistant' ? (props.item as { streaming: boolean }).streaming : false,
)
const attachments = computed(() =>
  props.item.kind === 'user' ? (props.item as { attachments?: Attachment[] }).attachments : undefined,
)

const toolName = computed(() =>
  props.item.kind === 'tool_call' ? (props.item as { name: string }).name : '',
)
const toolArgs = computed(() => {
  if (props.item.kind !== 'tool_call') return ''
  return JSON.stringify((props.item as { args: unknown }).args, null, 2)
})
const toolResultContent = computed(() =>
  props.item.kind === 'tool_result' ? (props.item as { content: string }).content : '',
)

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
  const it = props.item as { ts?: number; srcRun?: string }
  const ws = sessions.activeWorkspace
  const sid = sessions.activeId
  const logDir = settings.logDir || '~/.woca/logs'
  const lines = [
    '## woca debug context',
    '',
    `- time: ${it.ts ? new Date(it.ts).toISOString() : new Date().toISOString()}`,
    `- workspace: ${ws?.path || '(unknown)'}${ws?.name ? ` (${ws.name})` : ''}`,
    `- session: ${sid || '(unknown)'}`,
    `- run: ${it.srcRun || sessions.runId?.[sid || ''] || '(ended; see session trace)'}`,
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

  <!-- tool call -->
  <div v-else-if="item.kind === 'tool_call'" class="tool-card">
    <div class="tool-card-head">
      <Wrench :size="13" />
      <span class="tool-name">{{ toolName }}</span>
    </div>
    <pre class="tool-args">{{ toolArgs }}</pre>
  </div>

  <!-- tool result -->
  <div v-else class="tool-card tool-result">
    <button class="tool-card-head tool-toggle" @click="collapsed = !collapsed">
      <ChevronRight :size="13" class="tool-chevron" :class="{ open: !collapsed }" />
      <span class="tool-name">工具输出</span>
    </button>
    <pre v-if="!collapsed" class="tool-output">{{ toolResultContent }}</pre>
  </div>
</template>
