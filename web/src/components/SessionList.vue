<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronRight, Folder, Hand, Loader2, MessageSquare, Plus, Trash2 } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'
import { useConfirmStore } from '../stores/confirm'
import { useToastStore } from '../stores/toast'
import type { Session } from '../types'

// Sessions grouped by workspace with per-group drawers (collapse/expand).
// The status of each session shows on its row: spinner = running, hand =
// awaiting your decision.
const { t } = useI18n()
const toast = useToastStore()
const store = useSessionStore()
const confirmDialog = useConfirmStore()

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
  if (!iso) return t('sidebar.justNow')
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const now = new Date()
  const diff = now.getTime() - d.getTime()
  const min = Math.floor(diff / 60000)
  if (min < 1) return t('sidebar.justNow')
  if (min < 60) return t('sidebar.minutesAgo', { min })
  const hr = Math.floor(min / 60)
  if (hr < 24) return t('sidebar.hoursAgo', { hr })
  return t('sidebar.daysAgo', { days: Math.floor(hr / 24) })
}

// Deleting a session removes its transcript server-side; a live run refuses
// (removeSession throws — surfaced via toast).
async function removeSession(s: Session) {
  const title = s.title || t('sidebar.newSession')
  const ok = await confirmDialog.ask({
    title: t('sidebar.deleteSession'),
    body: t('sidebar.deleteSessionConfirm', { title }),
    confirmLabel: t('common.delete'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  try {
    await store.removeSession(s.id)
  } catch (e) {
    toast.error(e)
  }
}
</script>

<template>
  <div class="ws-groups">
    <div v-for="g in store.workspaceGroups" :key="g.key" class="ws-group">
      <div class="ws-group-head">
        <button class="ws-group-toggle" :aria-expanded="isOpen(g.key)" @click="toggle(g.key)">
          <ChevronRight :size="13" class="chevron" :class="{ open: isOpen(g.key) }" />
          <Folder :size="14" class="ws-folder-icon" />
          <span class="ws-group-name" :title="g.path || g.name">{{ g.name }}</span>
          <span class="ws-group-count">{{ g.sessions.length }}</span>
        </button>
        <button
          class="icon-btn ws-new-btn"
          :title="t('sidebar.newSessionHere')"
          :aria-label="t('sidebar.newSessionHere')"
          @click="store.createSession({ path: g.path, name: g.name })"
        >
          <Plus :size="14" />
        </button>
      </div>

      <Transition name="fade">
        <div v-if="isOpen(g.key)" class="ws-sessions">
        <div
          v-for="s in g.sessions"
          :key="s.id"
          class="session-row"
          :class="{ active: s.id === store.activeId, awaiting: store.runStateOf(s.id) === 'awaiting' }"
        >
          <button class="session-item" :class="{ active: s.id === store.activeId }" @click="store.setActive(s.id)">
            <Loader2
              v-if="store.runStateOf(s.id) === 'running'"
              :size="15"
              class="session-icon spinning"
              :title="t('sidebar.running')"
            />
            <Hand
              v-else-if="store.runStateOf(s.id) === 'awaiting'"
              :size="15"
              class="session-icon awaiting"
              :title="t('sidebar.awaitingDecision')"
            />
            <MessageSquare v-else :size="15" class="session-icon" />
            <span class="session-title">{{ s.title }}</span>
            <span class="session-time">{{ shortTime(s.updatedAt) }}</span>
          </button>
          <button
            class="session-del"
            :title="t('sidebar.deleteSession')"
            :aria-label="t('sidebar.deleteSession')"
            @click="removeSession(s)"
          >
            <Trash2 :size="12" />
          </button>
          </div>
        </div>
      </Transition>
    </div>
  </div>
</template>
