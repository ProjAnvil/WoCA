<script setup lang="ts">
import { computed, type Component } from 'vue'
import { useI18n } from 'vue-i18n'
import { Circle, CircleCheck, CircleDashed, CircleX, ListChecks } from 'lucide-vue-next'
import { useActivityStore } from '../../stores/activity'
import type { TodoItem } from '../../types'

// Read-only todo list (the plan's truth lives with the model — no manual
// editing). Status is encoded in icon shape + sr-only text, never color
// alone (UIUX §4.5); the in-progress row breathes like the running pill.
const props = defineProps<{ sid: string }>()

const { t } = useI18n()
const activity = useActivityStore()

const items = computed(() => activity.snapshotOf(props.sid)?.todos.items ?? [])
const progress = computed(() => activity.todoProgressOf(props.sid))

const ICONS: Record<TodoItem['status'], Component> = {
  pending: Circle,
  in_progress: CircleDashed,
  completed: CircleCheck,
  cancelled: CircleX,
}
</script>

<template>
  <div class="act-tab-body">
    <p v-if="progress.total" class="act-todo-progress">
      {{ t('activity.todoProgress', { done: progress.done, total: progress.total }) }}
    </p>
    <ul v-if="items.length" class="act-todo-list">
      <li
        v-for="(it, i) in items"
        :key="i"
        class="act-todo-row"
        :class="`s-${it.status}`"
        :title="it.content"
      >
        <component :is="ICONS[it.status]" :size="14" class="act-todo-icon" aria-hidden="true" />
        <span class="sr-only">{{ t(`activity.todoStatus.${it.status}`) }} — </span>
        <span class="act-todo-text">
          <!-- The present-tense label leads while the item runs (Codex parity). -->
          {{ it.status === 'in_progress' && it.active_form ? it.active_form : it.content }}
        </span>
      </li>
    </ul>
    <div v-else class="act-empty">
      <ListChecks :size="16" aria-hidden="true" />
      <span>{{ t('activity.todoEmpty') }}</span>
    </div>
  </div>
</template>

<style scoped>
.act-todo-progress {
  margin: 0 0 var(--space-2);
  padding: 0 var(--space-4);
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.act-todo-list {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: var(--gap-list);
}
.act-todo-row {
  display: flex;
  align-items: baseline;
  gap: var(--space-3);
  padding: var(--space-2) var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-sm);
  line-height: var(--lh-sm);
  color: var(--text-2);
}
.act-todo-row.s-in_progress {
  color: var(--text);
}
.act-todo-row.s-in_progress .act-todo-icon {
  color: var(--brand);
  animation: task-pill-breathe 1.6s ease-in-out infinite;
}
.act-todo-row.s-completed {
  color: var(--muted);
}
.act-todo-row.s-completed .act-todo-text {
  text-decoration: line-through;
}
.act-todo-row.s-completed .act-todo-icon {
  color: var(--ok);
}
.act-todo-row.s-cancelled {
  color: var(--muted);
}
.act-todo-row.s-cancelled .act-todo-icon {
  color: var(--muted);
}
.act-todo-row.s-pending .act-todo-icon {
  color: var(--muted);
}
.act-todo-icon {
  flex-shrink: 0;
  transform: translateY(2px);
}
.act-todo-text {
  min-width: 0;
  overflow-wrap: anywhere;
}
</style>
