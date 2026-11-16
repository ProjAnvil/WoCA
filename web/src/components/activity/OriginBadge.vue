<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

// The shared origin badge (§4.2): "main" (muted) or the subagent def name
// (brand-soft pill). Used at the head of approval cards and — from the
// shells task — the background tab's initiator column.
const props = defineProps<{
  /** "main" / "general-topology root asks" or a subagent def name. */
  origin: string
}>()

const { t } = useI18n()

const isMain = computed(() => !props.origin || props.origin === 'main')
const label = computed(() => (isMain.value ? t('chat.sourceMain') : props.origin))
</script>

<template>
  <span class="origin-badge" :class="{ main: isMain }">{{ label }}</span>
</template>

<style scoped>
.origin-badge {
  display: inline-flex;
  align-items: center;
  max-width: 160px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: 10.5px;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  line-height: 1;
  padding: 3px 8px;
  border-radius: var(--radius-full);
  background: var(--brand-soft);
  color: var(--brand-text);
}
/* Main-origin asks stay quiet: the badge is texture, not a signal. */
.origin-badge.main {
  background: transparent;
  color: var(--muted);
  opacity: 0.6;
  padding-inline-start: 0;
}
</style>
