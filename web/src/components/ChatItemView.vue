<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronRight, Wrench } from 'lucide-vue-next'
import type { ChatItem } from '../types'
import { renderMarkdown } from '../lib/markdown'
import { useSettingsStore } from '../stores/settings'

const props = defineProps<{ item: ChatItem }>()
const settings = useSettingsStore()

const isText = computed(() => props.item.kind === 'user' || props.item.kind === 'assistant')
const isUser = computed(() => props.item.kind === 'user')

const textContent = computed(() =>
  isText.value ? (props.item as { content: string }).content : '',
)
const textHtml = computed(() => (isText.value ? renderMarkdown(textContent.value) : ''))
const roleLabel = computed(() => (isUser.value ? '你' : 'woca'))
const streaming = computed(() =>
  props.item.kind === 'assistant' ? (props.item as { streaming: boolean }).streaming : false,
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
</script>

<template>
  <!-- user / assistant text -->
  <div v-if="isText" class="message" :class="isUser ? 'user' : 'assistant'">
    <div class="message-role">{{ roleLabel }}</div>
    <div class="message-bubble">
      <div class="markdown-body" v-html="textHtml"></div>
      <span v-if="streaming" class="cursor"></span>
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
