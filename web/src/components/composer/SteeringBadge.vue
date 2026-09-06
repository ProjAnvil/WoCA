<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Zap } from 'lucide-vue-next'

// Steered input: shown while a run is busy. Unlike the queue badge, this
// text already reached the agent's live input (Codex-style steering) — it
// is not editable or cancelable, only informational.
defineProps<{ count: number; text: string }>()
const { t } = useI18n()
</script>

<template>
  <div class="queue-badge steering-badge" role="status">
    <Zap :size="13" aria-hidden="true" />
    <span class="queue-badge-text">{{ t('chat.steeringBadge', { count }) }}</span>
    <span class="queue-badge-msg" :title="text">{{ text }}</span>
  </div>
</template>
