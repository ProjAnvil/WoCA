<script setup lang="ts">
import { ref, watch } from 'vue'
import { ChevronRight, Folder, MessageSquare, Plus } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'

const store = useSessionStore()

// Collapsed groups (default: expanded); switching the active session's
// workspace re-expands that group.
const collapsed = ref(new Set<string>())
function isOpen(key: string): boolean {
  return !collapsed.value.has(key)
}
function toggle(key: string) {
  const next = new Set(collapsed.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  collapsed.value = next
}

// Switching workspaces keeps the new active group open.
watch(
  () => store.activeWorkspaceKey,
  (key) => collapsed.value.delete(key),
)

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
  <div class="ws-groups">
    <div v-for="g in store.workspaceGroups" :key="g.key" class="ws-group">
      <div class="ws-group-head">
        <button class="ws-group-toggle" @click="toggle(g.key)">
          <ChevronRight :size="13" class="chevron" :class="{ open: isOpen(g.key) }" />
          <Folder :size="14" class="ws-folder-icon" />
          <span class="ws-group-name" :title="g.path || g.name">{{ g.name }}</span>
          <span class="ws-group-count">{{ g.sessions.length }}</span>
        </button>
        <button
          class="icon-btn ws-new-btn"
          title="在此工作区新建会话"
          @click="store.createSession({ path: g.path, name: g.name })"
        >
          <Plus :size="14" />
        </button>
      </div>

      <div v-if="isOpen(g.key)" class="ws-sessions">
        <button
          v-for="s in g.sessions"
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
    </div>
  </div>
</template>
