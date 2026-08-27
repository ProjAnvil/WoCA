<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronRight, Folder, Hand, Loader2, MessageSquare, Pin, Plus, Search, Trash2 } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'
import { useConfirmStore } from '../stores/confirm'
import { useToastStore } from '../stores/toast'
import { usePreference } from '../native/preferences'
import { buildStatusGroups, type StatusGroup, type StatusRow } from '../lib/sessionGroups'
import type { Session } from '../types'

// Cockpit sidebar: sessions from every workspace bucket into status groups
// (pinned / needs input / running / today / earlier) so attention comes
// first; workspace management lives in its own compact section below.

const emit = defineEmits<{ addWorkspace: [] }>()

const { t } = useI18n()
const toast = useToastStore()
const store = useSessionStore()
const confirmDialog = useConfirmStore()

/* ------------------------------ preferences ------------------------------- */

const pinnedPref = usePreference('woca.pref.pinnedSessions', '[]')
const collapsedPref = usePreference('woca.pref.sessionGroups', '["earlier"]')

function parseSet(raw: string): Set<string> {
  try {
    const v = JSON.parse(raw)
    return new Set(Array.isArray(v) ? v.map(String) : [])
  } catch {
    return new Set()
  }
}

const pinnedSet = computed(() => parseSet(pinnedPref.value))
const collapsedSet = computed(() => parseSet(collapsedPref.value))

function togglePin(id: string) {
  const next = new Set(pinnedSet.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  pinnedPref.value = JSON.stringify([...next])
}

function isOpen(key: string): boolean {
  return !collapsedSet.value.has(key)
}

function toggleGroup(key: string) {
  const next = new Set(collapsedSet.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  collapsedPref.value = JSON.stringify([...next])
}

/* ------------------------------ session rows ------------------------------ */

interface Row extends StatusRow<Session> {}

const allRows = computed<Row[]>(() =>
  (store.workspaceGroups ?? []).flatMap((g) =>
    g.sessions.map((s) => ({ session: s, workspace: g.name, run: store.runStateOf(s.id) })),
  ),
)

const statusGroups = computed<StatusGroup<Session>[]>(() =>
  buildStatusGroups(allRows.value, pinnedSet.value),
)

const workspaces = computed(() =>
  (store.workspaceGroups ?? []).map((g) => ({
    key: g.key,
    path: g.path,
    name: g.name,
    count: g.sessions.length,
    latest: g.sessions.reduce<Session | null>((a, b) => (!a || b.updatedAt > a.updatedAt ? b : a), null),
  })),
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

/* --------------------------------- filter --------------------------------- */

const filterVisible = ref(false)
const filterEl = ref<HTMLInputElement | null>(null)
const filter = ref('')
const filtering = computed(() => filter.value.trim().length > 0)

function toggleFilter() {
  // While typing, the toggle keeps the box (and focus); otherwise it flips.
  filterVisible.value = filtering.value ? true : !filterVisible.value
  if (filterVisible.value) void nextTick(() => filterEl.value?.focus())
}

function clearFilter() {
  filter.value = ''
}

function onSectionKeydown(e: KeyboardEvent) {
  const el = e.target as HTMLElement
  if (e.key === '/' && el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA' && !e.isComposing) {
    e.preventDefault()
    filterVisible.value = true
    void nextTick(() => filterEl.value?.focus())
  }
}

const filteredRows = computed<Row[]>(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return []
  return allRows.value.filter(
    (r) => r.session.title.toLowerCase().includes(q) || r.workspace.toLowerCase().includes(q),
  )
})

// One rendering path: filtering flattens into a single pseudo-group.
const view = computed<Array<StatusGroup<Session> & { flat: boolean }>>(() => {
  if (filtering.value) return [{ key: 'today', rows: filteredRows.value, flat: true }]
  return statusGroups.value.map((g) => ({ ...g, flat: false }))
})

/* --------------------------------- actions -------------------------------- */

function newSession() {
  store.createSession()
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
  <div class="side-cockpit">
    <div class="side-actions">
      <button class="side-new-btn" type="button" @click="newSession">
        <Plus :size="14" />
        <span>{{ t('sidebar.newSession') }}</span>
      </button>
      <button
        class="icon-btn"
        type="button"
        :title="t('sidebar.searchSessions')"
        :aria-label="t('sidebar.searchSessions')"
        @click="toggleFilter"
      >
        <Search :size="15" />
      </button>
    </div>

    <input
      v-if="filterVisible"
      ref="filterEl"
      v-model="filter"
      class="side-filter"
      type="text"
      :placeholder="t('sidebar.filterPlaceholder')"
      @keydown.esc="clearFilter"
    />

    <div class="ws-groups" @keydown="onSectionKeydown">
      <div v-for="g in view" :key="g.flat ? 'filter' : g.key" class="ws-group">
        <button
          v-if="!g.flat"
          class="ws-group-toggle"
          :aria-expanded="isOpen(g.key)"
          @click="toggleGroup(g.key)"
        >
          <ChevronRight :size="13" class="chevron" :class="{ open: isOpen(g.key) }" />
          <span class="grp-dot" :class="`grp-${g.key}`" aria-hidden="true" />
          <span class="ws-group-name">{{ t(`sidebar.groups.${g.key}`) }}</span>
          <span class="ws-group-count">{{ g.rows.length }}</span>
        </button>

        <div v-if="g.flat || isOpen(g.key)" class="ws-sessions">
          <div v-if="g.flat && !g.rows.length" class="side-empty">
            {{ t('sidebar.noMatch') }}
            <button class="side-empty-clear" type="button" @click="clearFilter">{{ t('common.clear') }}</button>
          </div>
          <div
            v-for="r in g.rows"
            :key="r.session.id"
            class="session-row"
            :class="{ active: r.session.id === store.activeId, awaiting: r.run === 'awaiting' }"
          >
            <button class="session-item" :class="{ active: r.session.id === store.activeId }" @click="store.setActive(r.session.id)">
              <Loader2
                v-if="r.run === 'running'"
                :size="15"
                class="session-icon spinning"
                :title="t('sidebar.running')"
              />
              <Hand
                v-else-if="r.run === 'awaiting'"
                :size="15"
                class="session-icon awaiting"
                :title="t('sidebar.awaitingDecision')"
              />
              <MessageSquare v-else :size="15" class="session-icon" />
              <span class="session-title">{{ r.session.title || t('sidebar.newSession') }}</span>
              <span class="session-ws">{{ r.workspace }}</span>
              <span class="session-time">{{ shortTime(r.session.updatedAt) }}</span>
            </button>
            <span class="session-actions">
              <button
                class="session-pin"
                :class="{ on: pinnedSet.has(r.session.id) }"
                :title="pinnedSet.has(r.session.id) ? t('sidebar.unpin') : t('sidebar.pin')"
                :aria-label="pinnedSet.has(r.session.id) ? t('sidebar.unpin') : t('sidebar.pin')"
                @click="togglePin(r.session.id)"
              >
                <Pin :size="12" />
              </button>
              <button
                class="session-del"
                :title="t('sidebar.deleteSession')"
                :aria-label="t('sidebar.deleteSession')"
                @click="removeSession(r.session)"
              >
                <Trash2 :size="12" />
              </button>
            </span>
          </div>
        </div>
      </div>
    </div>

    <section class="side-section ws-section">
      <div class="side-section-head">
        <span class="side-section-title">{{ t('sidebar.workspaces') }}</span>
        <button
          class="icon-btn"
          :title="t('sidebar.newWorkspace')"
          :aria-label="t('sidebar.newWorkspace')"
          @click="emit('addWorkspace')"
        >
          <Plus :size="15" />
        </button>
      </div>
      <div v-for="w in workspaces" :key="w.key" class="ws-mini">
        <button
          class="ws-mini-row"
          type="button"
          :title="w.path || w.name"
          :aria-label="w.name"
          @click="w.latest && store.setActive(w.latest.id)"
        >
          <Folder :size="13" class="ws-folder-icon" />
          <span class="ws-mini-name">{{ w.name }}</span>
          <span class="ws-group-count">{{ w.count }}</span>
        </button>
        <button
          class="icon-btn ws-new-btn"
          :title="t('sidebar.newSessionHere')"
          :aria-label="t('sidebar.newSessionHere')"
          @click="store.createSession({ path: w.path, name: w.name })"
        >
          <Plus :size="13" />
        </button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.side-cockpit {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.side-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.side-new-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  flex: 1;
  height: 28px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface);
  color: var(--text-2);
  font-family: inherit;
  font-size: var(--text-sm);
  font-weight: 500;
  cursor: pointer;
  transition:
    background var(--dur-fast) var(--ease-out),
    color var(--dur-fast) var(--ease-out);
}
.side-new-btn:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.side-new-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}

.side-filter {
  width: 100%;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg);
  color: var(--text);
  font: inherit;
  font-size: var(--text-sm);
}
.side-filter:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}

/* Status tone dots on group headers (running breathes). */
.grp-dot {
  width: 7px;
  height: 7px;
  flex-shrink: 0;
  border-radius: var(--radius-full);
  background: var(--muted);
}
.grp-dot.grp-pinned,
.grp-dot.grp-running {
  background: var(--brand);
}
.grp-dot.grp-needsInput {
  background: var(--warn);
}
.grp-dot.grp-earlier {
  background: var(--faint);
}
.grp-dot.grp-running {
  animation: grp-breathe 1.6s ease-in-out infinite;
}
@keyframes grp-breathe {
  0%,
  100% {
    opacity: 0.4;
  }
  50% {
    opacity: 1;
  }
}

/* Workspace hint inside session rows + awaiting tint. */
.session-ws {
  max-width: 90px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--faint);
  font-size: var(--text-xs);
}
.session-row.awaiting :deep(.session-item) {
  background: var(--warn-soft);
}

.session-actions {
  display: none;
  align-items: center;
  gap: 2px;
}
.session-row:hover .session-actions,
.session-row:focus-within .session-actions {
  display: inline-flex;
}
.session-pin {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border: none;
  border-radius: var(--radius-xs);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
}
.session-pin:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.session-pin:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.session-pin.on {
  color: var(--brand-text);
}

.side-empty {
  padding: var(--space-4) var(--space-2);
  color: var(--muted);
  font-size: var(--text-sm);
  text-align: center;
}
.side-empty-clear {
  margin-left: var(--space-2);
  border: none;
  background: none;
  color: var(--brand-text);
  font: inherit;
  cursor: pointer;
  text-decoration: underline;
}

/* Compact workspace management rows. */
.ws-section {
  margin-top: var(--space-2);
}
.ws-mini {
  display: flex;
  align-items: center;
  gap: 2px;
}
.ws-mini-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex: 1;
  min-width: 0;
  height: 26px;
  padding: 0 var(--space-2);
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  font-family: inherit;
  font-size: var(--text-sm);
  cursor: pointer;
}
.ws-mini-row:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.ws-mini-row:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: -2px;
}
.ws-mini-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  text-align: left;
}
</style>
