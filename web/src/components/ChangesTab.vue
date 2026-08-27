<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Check, ChevronRight, FilePlus2, FileX2, MessageSquarePlus, RefreshCw, RotateCcw, Undo2 } from 'lucide-vue-next'
import WSkeleton from './ui/WSkeleton.vue'
import { useWorkspaceStore } from '../stores/workspace'
import { useConfirmStore } from '../stores/confirm'
import { useToastStore } from '../stores/toast'
import { useSessionStore } from '../stores/sessions'
import { dropLastTurn, type WorkspaceChange } from '../api/client'

// The Changes review tab (P2-4): every pending (undoable) workspace change
// as a stacked unified-diff card, per-file and whole-workspace rollback
// (checkpoint semantics), the conversation leg of rollback, and parked
// inline review comments that ride the next run (P2-2).
const { t } = useI18n()
const store = useWorkspaceStore()
const confirmDialog = useConfirmStore()
const toast = useToastStore()
const sessions = useSessionStore()

const expanded = ref(new Set<string>())
const commentDraft = ref<Record<string, string>>({})
const commentingLine = ref('')

function isOpen(path: string): boolean {
  return expanded.value.has(path)
}
function toggle(path: string) {
  const next = new Set(expanded.value)
  if (next.has(path)) next.delete(path)
  else next.add(path)
  expanded.value = next
}

async function rollbackAll() {
  const ok = await confirmDialog.ask({
    title: t('workspace.changes.rollbackAllTitle'),
    body: t('workspace.changes.rollbackAllBody', { count: store.undoSteps }),
    confirmLabel: t('workspace.changes.rollback'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  await store.undo()
  toast.success(t('workspace.changes.rolledBack'))
}

async function rollbackFile(c: WorkspaceChange) {
  const ok = await confirmDialog.ask({
    title: t('workspace.changes.rollbackFileTitle'),
    body: t('workspace.changes.rollbackFileBody', { path: c.path }),
    confirmLabel: t('workspace.changes.rollback'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  await store.undo(c.path)
  toast.success(t('workspace.changes.rolledBack'))
}

async function dropConversation() {
  const sid = sessions.activeId
  if (!sid) return
  const ok = await confirmDialog.ask({
    title: t('workspace.changes.dropTurnTitle'),
    body: t('workspace.changes.dropTurnBody'),
    confirmLabel: t('workspace.changes.dropTurn'),
    cancelLabel: t('common.cancel'),
    tone: 'danger',
  })
  if (!ok) return
  try {
    await dropLastTurn(sid)
    await sessions.loadSession(sid)
    toast.success(t('workspace.changes.turnDropped'))
  } catch (e) {
    toast.error(e)
  }
}

/* ------------------------- inline review comments ------------------------- */

function lineKey(path: string, idx: number): string {
  return `${path}#${idx}`
}

function diffLines(c: WorkspaceChange): string[] {
  return c.diff ? c.diff.split('\n').filter((l) => l.startsWith('+') || l.startsWith('-') || l.startsWith(' ')) : []
}

/** Approximate line number in the NEW file for a diff body line. */
function newLineNo(lines: string[], idx: number): number {
  let n = 0
  for (let i = 0; i <= idx; i++) {
    if (!lines[i].startsWith('-')) n++
  }
  return n
}

function submitComment(c: WorkspaceChange, idx: number) {
  const text = (commentDraft.value[lineKey(c.path, idx)] ?? '').trim()
  if (text) store.addComment(c.path, newLineNo(diffLines(c), idx), text)
  commentDraft.value[lineKey(c.path, idx)] = ''
  commentingLine.value = ''
}

function cancelComment(k: string) {
  commentDraft.value[k] = ''
  commentingLine.value = ''
}

const commentCount = computed(() => store.pendingComments.length)

function clearComments() {
  for (const c of [...store.pendingComments]) store.removeComment(c.file, c.line)
}
</script>

<template>
  <div class="changes-tab">
    <div class="changes-toolbar">
      <span class="changes-count">
        {{ t('workspace.changes.pendingCount', { count: store.changes.length }) }}
      </span>
      <span class="changes-toolbar-actions">
        <button
          v-if="store.changes.length"
          class="changes-act danger"
          type="button"
          :title="t('workspace.changes.rollbackAllTitle')"
          @click="rollbackAll"
        >
          <Undo2 :size="13" /> {{ t('workspace.changes.rollbackAll') }}
        </button>
        <button class="changes-act" type="button" :title="t('workspace.changes.dropTurnTitle')" @click="dropConversation">
          <RotateCcw :size="13" /> {{ t('workspace.changes.dropTurn') }}
        </button>
        <button class="icon-btn" type="button" :title="t('common.refresh')" :aria-label="t('common.refresh')" @click="store.refreshChanges()">
          <RefreshCw :size="14" />
        </button>
      </span>
    </div>

    <div v-if="store.changesLoading" class="changes-skeleton" role="status" aria-busy="true">
      <span class="sr-only">{{ t('common.loading') }}</span>
      <WSkeleton v-for="i in 3" :key="i" variant="rect" height="42px" />
    </div>
    <div v-else-if="store.changesError" class="changes-error">{{ store.changesError }}</div>
    <div v-else-if="!store.changes.length" class="changes-empty">
      <Check :size="18" aria-hidden="true" />
      <span>{{ t('workspace.changes.empty') }}</span>
    </div>
    <template v-else>
      <div v-if="commentCount" class="changes-comments-tray">
        <MessageSquarePlus :size="13" aria-hidden="true" />
        <span>{{ t('workspace.changes.commentsQueued', { count: commentCount }) }}</span>
        <button class="changes-act" type="button" @click="clearComments">{{ t('common.clear') }}</button>
      </div>

      <div v-for="c in store.changes" :key="c.path" class="diff-card">
        <div class="diff-head-row">
          <button class="diff-head" type="button" :aria-expanded="isOpen(c.path)" @click="toggle(c.path)">
            <ChevronRight :size="13" class="diff-chevron" :class="{ open: isOpen(c.path) }" aria-hidden="true" />
            <span class="diff-path">
              <FilePlus2 v-if="c.created" :size="12" class="diff-flag created" aria-hidden="true" />
              <FileX2 v-else-if="c.deleted" :size="12" class="diff-flag deleted" aria-hidden="true" />
              {{ c.path }}
            </span>
            <span class="diff-stat">
              <span class="add">+{{ c.added }}</span>
              <span class="del">−{{ c.removed }}</span>
            </span>
          </button>
          <button
            class="diff-rollback"
            type="button"
            :title="t('workspace.changes.rollbackFileTitle')"
            :aria-label="t('workspace.changes.rollbackFileTitle')"
            @click="rollbackFile(c)"
          >
            <Undo2 :size="12" />
          </button>
        </div>
        <div v-if="isOpen(c.path)" class="diff-body">
          <div v-if="c.binary" class="diff-binary">{{ t('workspace.changes.binary') }}</div>
          <template v-else>
            <template v-for="(line, i) in diffLines(c)" :key="i">
              <div class="diff-line" :class="line.charAt(0)">
                <span class="diff-line-text">{{ line.slice(1) }}</span>
                <button
                  v-if="line.startsWith('+') || line.startsWith('-')"
                  class="diff-line-comment"
                  type="button"
                  :aria-label="t('workspace.changes.commentLine')"
                  :title="t('workspace.changes.commentLine')"
                  @click="commentingLine = lineKey(c.path, i)"
                >
                  <MessageSquarePlus :size="11" />
                </button>
              </div>
              <div v-if="commentingLine === lineKey(c.path, i)" class="diff-comment-box">
                <input
                  v-model="commentDraft[lineKey(c.path, i)]"
                  class="diff-comment-input"
                  type="text"
                  :placeholder="t('workspace.changes.commentPlaceholder')"
                  @keydown.enter.prevent="submitComment(c, i)"
                  @keydown.esc.prevent="cancelComment(lineKey(c.path, i))"
                />
              </div>
            </template>
          </template>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.changes-tab {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-3);
  overflow-y: auto;
  flex: 1;
}
.changes-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}
.changes-count {
  font-size: var(--text-sm);
  color: var(--text-2);
}
.changes-toolbar-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.changes-act {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 3px 9px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface);
  color: var(--text-2);
  font-family: inherit;
  font-size: var(--text-xs);
  cursor: pointer;
}
.changes-act:hover {
  background: var(--surface-hover);
  color: var(--text);
}
.changes-act.danger {
  color: var(--danger-text);
  border-color: color-mix(in srgb, var(--danger) 35%, var(--border));
}
.changes-act:focus-visible,
.diff-line-comment:focus-visible,
.diff-rollback:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.changes-skeleton {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.changes-error {
  color: var(--danger-text);
  font-size: var(--text-sm);
}
.changes-empty {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--ok-text);
  font-size: var(--text-sm);
  padding: var(--space-6) 0;
  justify-content: center;
}
.changes-comments-tray {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  border: 1px solid color-mix(in srgb, var(--brand) 40%, var(--border));
  border-radius: var(--radius);
  background: var(--brand-soft);
  color: var(--brand-text);
  font-size: var(--text-xs);
}

.diff-card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface);
  overflow: hidden;
}
.diff-head-row {
  display: flex;
  align-items: stretch;
}
.diff-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex: 1;
  min-width: 0;
  padding: var(--space-2) var(--space-3);
  border: none;
  background: transparent;
  color: var(--text-2);
  font: inherit;
  font-size: var(--text-xs);
  font-family: var(--font-mono);
  text-align: left;
  cursor: pointer;
}
.diff-head:hover {
  background: var(--surface-hover);
}
.diff-head:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: -2px;
}
.diff-rollback {
  display: inline-flex;
  align-items: center;
  padding: 0 var(--space-2);
  border: none;
  border-left: 1px solid var(--border);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
}
.diff-rollback:hover {
  color: var(--danger-text);
  background: var(--surface-hover);
}
.diff-chevron {
  transition: transform var(--dur-base) var(--ease-out);
  color: var(--muted);
}
.diff-chevron.open {
  transform: rotate(90deg);
}
.diff-flag.created {
  color: var(--ok-text);
}
.diff-flag.deleted {
  color: var(--danger-text);
}
.diff-path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.diff-stat {
  display: inline-flex;
  gap: 6px;
  font-variant-numeric: tabular-nums;
}
.diff-stat .add {
  color: var(--ok-text);
}
.diff-stat .del {
  color: var(--danger-text);
}
.diff-body {
  border-top: 1px solid var(--border);
  max-height: 320px;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: 11.5px;
  line-height: 1.5;
}
.diff-binary {
  padding: var(--space-3);
  color: var(--muted);
}
.diff-line {
  display: flex;
  align-items: flex-start;
  gap: var(--space-1);
  padding: 0 var(--space-2);
  white-space: pre-wrap;
  word-break: break-all;
}
.diff-line-text {
  flex: 1;
}
.diff-line.\+ {
  background: color-mix(in srgb, var(--ok) 12%, transparent);
}
.diff-line.\- {
  background: color-mix(in srgb, var(--danger) 12%, transparent);
}
.diff-line-comment {
  display: none;
  border: none;
  background: transparent;
  color: var(--muted);
  cursor: pointer;
  padding: 1px;
}
.diff-line:hover .diff-line-comment {
  display: inline-flex;
}
.diff-line-comment:hover {
  color: var(--brand-text);
}
.diff-comment-box {
  padding: var(--space-1) var(--space-2);
  background: var(--surface-2);
}
.diff-comment-input {
  width: 100%;
  padding: var(--space-1) var(--space-2);
  border: 1px solid color-mix(in srgb, var(--brand) 45%, var(--border));
  border-radius: var(--radius-xs);
  background: var(--bg);
  color: var(--text);
  font: inherit;
  font-size: 11.5px;
}
.diff-comment-input:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
</style>
