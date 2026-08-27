<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import WModal from './WModal.vue'
import WButton from './WButton.vue'
import { useConfirmStore } from '../../stores/confirm'

const { t } = useI18n()
const confirm = useConfirmStore()

const typed = ref('')
watch(
  () => confirm.req,
  () => {
    typed.value = ''
  },
)

const blocked = computed(() => {
  const req = confirm.req
  return !!req?.requireText && typed.value.trim() !== req.requireText
})

function onConfirm() {
  if (!blocked.value) confirm.settle(true)
}
</script>

<template>
  <WModal :open="!!confirm.req" :title="confirm.req?.title" @close="confirm.settle(false)">
    <div v-if="confirm.req" class="w-confirm">
      <h3 class="w-confirm-title">{{ confirm.req.title }}</h3>
      <p v-if="confirm.req.body" class="w-confirm-body">{{ confirm.req.body }}</p>
      <input
        v-if="confirm.req.requireText"
        v-model="typed"
        class="w-confirm-input"
        type="text"
        :placeholder="confirm.req.requireText"
        autocomplete="off"
        spellcheck="false"
        :aria-label="confirm.req.requireText"
        @keydown.enter.prevent="onConfirm"
      />
      <div class="w-confirm-actions">
        <WButton variant="secondary" size="sm" @click="confirm.settle(false)">
          {{ confirm.req.cancelLabel || t('common.cancel') }}
        </WButton>
        <WButton :variant="confirm.req.tone === 'danger' ? 'danger' : 'primary'" size="sm" :disabled="blocked" @click="onConfirm">
          {{ confirm.req.confirmLabel || t('common.confirm') }}
        </WButton>
      </div>
    </div>
  </WModal>
</template>

<style scoped>
.w-confirm {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  padding: var(--pad-modal);
}
.w-confirm-title {
  margin: 0;
  font-size: var(--text-md);
  line-height: var(--lh-md);
  font-weight: 600;
  color: var(--text);
}
.w-confirm-body {
  margin: 0;
  font-size: var(--text-base);
  line-height: var(--lh-base);
  color: var(--text-2);
  overflow-wrap: anywhere;
}
.w-confirm-input {
  width: 100%;
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg);
  color: var(--text);
  font: inherit;
}
.w-confirm-input:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.w-confirm-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-3);
}
</style>
