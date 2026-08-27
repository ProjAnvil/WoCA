<script setup lang="ts">
withDefaults(defineProps<{ hint?: string }>(), { hint: '' })
</script>

<template>
  <div class="w-empty">
    <div class="w-empty-mark" aria-hidden="true"><slot name="icon">w</slot></div>
    <div class="w-empty-body"><slot /></div>
    <p v-if="hint" class="w-empty-hint">{{ hint }}</p>
    <div v-if="$slots.actions" class="w-empty-actions"><slot name="actions" /></div>
  </div>
</template>

<style scoped>
.w-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-4);
  padding: var(--space-10) var(--space-5);
  max-width: var(--chat-col);
  margin: 0 auto;
  text-align: center;
}
.w-empty-mark {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border-radius: var(--radius-md);
  background: var(--brand);
  color: var(--on-brand);
  font-size: 20px;
  font-weight: 600;
  font-family: var(--font-sans);
}
.w-empty-body {
  color: var(--text-2);
  font-size: var(--text-md);
  line-height: var(--lh-md);
}
.w-empty-hint {
  margin: 0;
  color: var(--muted);
  font-size: var(--text-sm);
  line-height: var(--lh-sm);
  max-width: 48ch;
}
.w-empty-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: var(--space-3);
  margin-top: var(--space-2);
}
</style>
