<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Clock, Pencil, X } from 'lucide-vue-next'

// Parked next message: shown while a run is busy; flushes automatically
// when the run ends (owned by Composer's useMessageQueue).
defineProps<{ text: string }>()
const emit = defineEmits<{ edit: []; cancel: [] }>()
const { t } = useI18n()
</script>

<template>
  <div class="queue-badge" role="status">
    <Clock :size="13" aria-hidden="true" />
    <span class="queue-badge-text">{{ t('chat.queuedBadge') }}</span>
    <span class="queue-badge-msg" :title="text">{{ text }}</span>
    <button
      class="queue-btn"
      type="button"
      :aria-label="t('chat.queueEdit')"
      :title="t('chat.queueEdit')"
      @click="emit('edit')"
    >
      <Pencil :size="12" />
    </button>
    <button
      class="queue-btn"
      type="button"
      :aria-label="t('chat.queueCancel')"
      :title="t('chat.queueCancel')"
      @click="emit('cancel')"
    >
      <X :size="12" />
    </button>
  </div>
</template>
