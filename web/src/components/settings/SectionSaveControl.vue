<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useServerSettingsStore, type SectionId } from '../../stores/serverSettings'

// Per-section save affordance (F2 · variant A): enabled + highlighted while
// the section is dirty, visible-but-disabled when clean. The in-flight
// disable is a double-submit guard only — it never gates validation.
const props = defineProps<{ section: SectionId }>()
const { t } = useI18n()
const store = useServerSettingsStore()

const dirty = computed(() => store.sectionDirty(props.section))
const savingThis = computed(() => store.savingSection === props.section)
</script>

<template>
  <span class="sec-actions" :data-section="section">
    <!-- The visual dirty dot needs no name of its own: the sr-only text
         carries the state for assistive tech. -->
    <span v-if="dirty" class="sr-only">{{ t('settings.section.unsaved') }}</span>
    <span class="dirty-dot" :class="{ on: dirty }" aria-hidden="true"></span>
    <button
      class="btn-save"
      :class="{ dirty }"
      type="button"
      :disabled="!dirty || savingThis"
      data-testid="section-save"
      @click="store.saveSection(section)"
    >
      {{ savingThis ? t('settings.shared.saving') : t('settings.section.save') }}
    </button>
  </span>
</template>

<style scoped>
.sec-actions {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
/* Dirty marker: the existing --warn token already carries "attention"
   semantics elsewhere in the panel (installing state, restart hint). */
.dirty-dot {
  display: none;
  width: 6px;
  height: 6px;
  border-radius: 999px;
  background: var(--warn);
  flex: none;
}
.dirty-dot.on {
  display: inline-block;
}
/* Clean = quiet outline, disabled; dirty = the accent fill the panel's
   active states already use (nav chips, primary buttons). */
.btn-save {
  flex: none;
  min-height: 24px; /* repo touch-target floor */
  padding: 3px 12px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  background: var(--surface);
  color: var(--text-2);
  font: inherit;
  font-size: 12px;
  line-height: 18px;
  cursor: pointer;
  transition:
    background 0.12s ease,
    border-color 0.12s ease,
    color 0.12s ease;
}
.btn-save:disabled {
  opacity: 0.45;
  cursor: default;
}
.btn-save.dirty:not(:disabled) {
  background: var(--accent);
  border-color: var(--accent);
  color: var(--on-accent);
}
.btn-save.dirty:not(:disabled):hover {
  background: var(--accent-hover);
}
.btn-save:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
</style>
