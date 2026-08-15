<script setup lang="ts">
import { MessageSquare } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'

const store = useSessionStore()

function shortTime(iso: string): string {
  if (!iso) return '刚刚'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const now = new Date()
  const diff = now.getTime() - d.getTime()
  const min = Math.floor(diff / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr} 小时前`
  return `${Math.floor(hr / 24)} 天前`
}
</script>

<template>
  <div>
    <button
      v-for="s in store.sessions"
      :key="s.id"
      class="session-item"
      :class="{ active: s.id === store.activeId }"
      @click="store.setActive(s.id)"
    >
      <MessageSquare :size="15" class="session-icon" />
      <span class="session-title">{{ s.title }}</span>
      <span class="session-time">{{ shortTime(s.updatedAt) }}</span>
    </button>
  </div>
</template>
