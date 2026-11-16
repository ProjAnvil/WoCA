<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useFocusTrap } from '../../composables/useFocusTrap'
import { toErrorMessage } from '../../lib/errors'
import { listGrants, revokeGrant } from '../../api/client'
import type { GrantInfo } from '../../types'

// The session's granted rule keys ("allow for session" answers, §4.1):
// one row per rule key with a revoke button. Opened from the approval
// stack's "N granted · manage" footnote (and, from the activity task, the
// activity panel's grants chip).
const props = defineProps<{
  open: boolean
  /** The session whose grants are listed ('' = the base policy). */
  session: string
}>()
const emit = defineEmits<{ close: []; revoked: [key: string] }>()

const { t } = useI18n()

const grants = ref<GrantInfo[]>([])
const loading = ref(false)
const error = ref('')
const revoking = ref('')

const panel = ref<HTMLElement | null>(null)
const openRef = computed(() => props.open)
useFocusTrap(panel, openRef, { esc: () => emit('close') })

async function refresh() {
  loading.value = true
  error.value = ''
  try {
    const resp = await listGrants(props.session)
    grants.value = resp.grants ?? []
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    loading.value = false
  }
}

watch(
  () => props.open,
  (open) => {
    if (open) void refresh()
  },
  { immediate: true },
)

async function revoke(key: string) {
  if (revoking.value) return
  revoking.value = key
  try {
    await revokeGrant(key, props.session)
    await refresh()
    emit('revoked', key)
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    revoking.value = ''
  }
}
</script>

<template>
  <Transition name="panel-pop">
    <div v-if="open" class="overlay hitl-grants-scrim" @click.self="emit('close')">
      <div ref="panel" class="card hitl-grants-card" role="dialog" aria-modal="true" :aria-label="t('chat.grantedRules', { count: grants.length })" tabindex="-1">
        <div class="card-head">
          <span class="card-title">{{ t('chat.grantedRules', { count: grants.length }) }}</span>
          <button class="icon-btn" type="button" :aria-label="t('common.close')" @click="emit('close')">✕</button>
        </div>
        <div class="card-body">
          <div v-if="error" class="hitl-grants-error" role="alert">{{ error }}</div>
          <div v-else-if="loading" class="hitl-grants-empty">{{ t('common.loading') }}</div>
          <div v-else-if="!grants.length" class="hitl-grants-empty">{{ t('chat.grantsEmpty') }}</div>
          <div v-else class="hitl-grants-list" role="list">
            <div
              v-for="g in grants"
              :key="g.key"
              class="hitl-grants-row"
              role="listitem"
              :data-testid="`grants-row-${g.key}`"
            >
              <span class="hitl-grants-key" :title="g.key">{{ g.key }}</span>
              <button
                class="btn grant-revoke"
                type="button"
                :disabled="revoking === g.key"
                :title="t('chat.revokeGrant')"
                :aria-label="`${t('chat.revokeGrant')} ${g.key}`"
                @click="revoke(g.key)"
              >
                {{ t('chat.remove') }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
/* Layered above the approval stack it opens from (overlay is z-40). */
.hitl-grants-scrim {
  z-index: 50;
}
.hitl-grants-card {
  width: 480px;
  max-width: 100%;
  max-height: 70vh;
  display: flex;
  flex-direction: column;
}
.hitl-grants-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.hitl-grants-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 6px 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.hitl-grants-key {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}
.hitl-grants-empty {
  color: var(--muted);
  font-size: var(--text-sm);
  padding: var(--space-2) 0;
}
.hitl-grants-error {
  color: var(--danger-text);
  font-size: var(--text-xs);
}
.grant-revoke {
  flex-shrink: 0;
}
</style>
