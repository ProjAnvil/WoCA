<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { X } from 'lucide-vue-next'
import type { Attachment } from '../../types'

// Image attachments parked for the next send: thumbnails with per-item
// remove, plus the tray-level validation error (size/count).
defineProps<{ attachments: Attachment[]; error: string }>()
const emit = defineEmits<{ remove: [id: string] }>()
const { t } = useI18n()
</script>

<template>
  <div v-if="attachments.length || error" class="attach-tray">
    <div v-for="a in attachments" :key="a.id" class="attach-chip">
      <img :src="a.dataUrl" :alt="a.name" class="attach-thumb" />
      <button
        class="attach-remove"
        :title="t('chat.remove')"
        :aria-label="t('chat.remove')"
        @click="emit('remove', a.id)"
      >
        <X :size="10" />
      </button>
      <span class="attach-name">{{ a.name }}</span>
    </div>
    <span v-if="error" class="attach-error">{{ error }}</span>
  </div>
</template>
