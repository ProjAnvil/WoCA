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
        <X :size="12" aria-hidden="true" />
      </button>
      <span class="attach-name">{{ a.name }}</span>
    </div>
    <span v-if="error" class="attach-error">{{ error }}</span>
  </div>
</template>

<style scoped>
/* Hit-target floor (w320 audit): the shared 17px glyph in composer.css is
   under the 24px minimum. Scoped override keeps the fix local to the tray
   without editing the shared sheet. */
.attach-remove {
  width: 24px;
  height: 24px;
  top: -8px;
  right: -8px;
}
</style>
