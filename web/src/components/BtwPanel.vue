<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Eraser, X } from 'lucide-vue-next'
import { useBtwStore } from '../stores/btw'

// The /btw side-question panel: a 360px overlay beside the right sidebar.
// Questions come from the composer's "/btw " prefix; closing forgets
// everything (the exchange was never part of the transcript anyway).
const { t } = useI18n()
const btw = useBtwStore()
</script>

<template>
  <aside v-if="btw.open" class="btw-panel" aria-label="btw">
    <header class="btw-head">
      <span class="btw-title">{{ t('btw.title') }}</span>
      <span v-if="btw.running" class="btw-live" role="status">{{ t('btw.thinking') }}</span>
      <span class="btw-head-actions">
        <button class="icon-btn" type="button" :title="t('btw.clear')" :aria-label="t('btw.clear')" @click="btw.clear()">
          <Eraser :size="14" />
        </button>
        <button class="icon-btn" type="button" :title="t('common.close')" :aria-label="t('common.close')" @click="btw.hide()">
          <X :size="15" />
        </button>
      </span>
    </header>
    <div class="btw-body">
      <div v-if="btw.error" class="btw-error">{{ btw.error }}</div>
      <div v-if="!btw.entries.length && !btw.error" class="btw-hint">{{ t('btw.hint') }}</div>
      <div v-for="(e, i) in btw.entries" :key="i" class="btw-entry" :class="e.role">{{ e.content }}</div>
    </div>
  </aside>
</template>

<style scoped>
.btw-panel {
  position: fixed;
  top: 0;
  right: 0;
  bottom: 0;
  width: min(360px, 90vw);
  z-index: 210;
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-left: 1px solid var(--border);
  box-shadow: var(--shadow-overlay);
}
.btw-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border);
}
.btw-title {
  font-size: var(--text-sm);
  font-weight: 600;
  color: var(--text);
}
.btw-live {
  font-size: var(--text-xs);
  color: var(--brand-text);
}
.btw-head-actions {
  margin-left: auto;
  display: inline-flex;
  gap: var(--space-1);
}
.btw-body {
  flex: 1;
  overflow-y: auto;
  padding: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.btw-error {
  color: var(--danger-text);
  font-size: var(--text-sm);
}
.btw-hint {
  color: var(--muted);
  font-size: var(--text-sm);
  line-height: var(--lh-sm);
}
.btw-entry {
  font-size: var(--text-sm);
  line-height: var(--lh-sm);
  white-space: pre-wrap;
  border-radius: var(--radius);
  padding: var(--space-3);
}
.btw-entry.user {
  background: var(--surface-2);
  color: var(--text);
  align-self: flex-end;
  max-width: 88%;
}
.btw-entry.assistant {
  color: var(--text-2);
}
</style>
