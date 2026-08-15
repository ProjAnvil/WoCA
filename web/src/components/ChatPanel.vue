<script setup lang="ts">
import { computed, ref } from 'vue'
import { Send, Square } from 'lucide-vue-next'
import ChatItemView from './ChatItemView.vue'
import { useSessionStore } from '../stores/sessions'

const store = useSessionStore()
const draft = ref('')

const busy = computed(() => store.activeStatus === 'running' || store.activeStatus === 'awaiting')
const canSend = computed(() => draft.value.trim().length > 0 && !busy.value)

function send() {
  const text = draft.value.trim()
  if (!text) return
  draft.value = ''
  void store.send(text)
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    send()
  }
}
</script>

<template>
  <div class="chat">
    <header class="chat-header">
      <span class="chat-title">{{ store.activeSession?.title ?? 'woca' }}</span>
      <span v-if="store.activeStatus === 'running'" class="run-badge">运行中…</span>
      <span v-else-if="store.activeStatus === 'awaiting'" class="run-badge">等待你的确认…</span>
    </header>

    <div class="chat-messages">
      <ChatItemView v-for="item in store.activeItems" :key="item.id" :item="item" />
      <div v-if="store.activeStatus === 'error'" class="error-banner">
        {{ store.activeError }}
      </div>
      <div v-if="store.activeItems.length === 0 && !busy" class="empty-hint">
        开始一个新对话吧。
      </div>
    </div>

    <div class="chat-input">
      <div class="chat-input-inner">
        <textarea
          v-model="draft"
          rows="1"
          placeholder="给 woca 发送消息…"
          @keydown="onKeydown"
        ></textarea>
        <button v-if="!busy" class="send-btn" :disabled="!canSend" @click="send">
          <Send :size="16" />
        </button>
        <button v-else class="send-btn stop" title="停止" @click="store.cancel()">
          <Square :size="14" />
        </button>
      </div>
    </div>
  </div>
</template>
