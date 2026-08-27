<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Network, RefreshCw } from 'lucide-vue-next'
import type { EdgeBridge } from '../../api/client'

defineProps<{ bridges: EdgeBridge[]; current?: string }>()
const emit = defineEmits<{ refresh: [] }>()

const { t } = useI18n()
const addTarget = ref('')
const addError = ref('')

// The edge sets the routing cookie server-side (HttpOnly) and bounces
// back to / — a full navigation re-fetches the SPA from the new bridge,
// so no in-app state needs tearing down here.
function pick(id: string) {
  window.location.href = `/edge/select?bridge=${encodeURIComponent(id)}`
}

// "Add" covers two shapes the inventory list cannot express: a bridge id
// this edge knows but no longer lists, or a whole other access point (its
// URL navigates cross-origin — that edge's own selection flow takes over).
function addTargetValue() {
  const v = addTarget.value.trim()
  addError.value = ''
  if (!v) return
  if (/^https?:\/\//i.test(v)) {
    let u: URL
    try {
      u = new URL(v)
    } catch {
      addError.value = t('settings.edge.addInvalid')
      return
    }
    window.location.href = u.origin
    return
  }
  if (!/^[\w.-]+$/.test(v)) {
    addError.value = t('settings.edge.addInvalid')
    return
  }
  pick(v)
}
</script>

<template>
  <!-- Access point (edge scenario ④): pick which bridge this origin
       commands. The list comes from the edge itself, pre-auth. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Network :size="13" style="vertical-align: -2px" /> {{ t('settings.edge.title') }}
      <button class="icon-btn edge-refresh" :title="t('settings.edge.refresh')" @click="emit('refresh')">
        <RefreshCw :size="13" />
      </button>
    </div>
    <p class="mem-intro">{{ t('settings.edge.intro') }}</p>

    <div class="edge-list">
      <button
        v-for="b in bridges"
        :key="b.bridge_id"
        class="edge-bridge"
        :class="{ current: b.bridge_id === current }"
        :data-testid="`edge-bridge-${b.bridge_id}`"
        @click="pick(b.bridge_id)"
      >
        <span class="edge-dot" :class="{ live: b.live }" data-testid="edge-dot" />
        <span class="edge-id">{{ b.bridge_id }}</span>
        <span class="edge-state">{{ b.live ? t('settings.edge.live') : t('settings.edge.offline') }}</span>
        <span v-if="b.bridge_id === current" class="edge-current">{{ t('settings.edge.current') }}</span>
      </button>
    </div>

    <div class="setting-row edge-add">
      <div>
        <div class="row-label">{{ t('settings.edge.addLabel') }}</div>
        <div class="row-hint">{{ t('settings.edge.addHint') }}</div>
        <div v-if="addError" class="row-hint" style="color: var(--danger)">{{ addError }}</div>
      </div>
      <div class="edge-add-controls">
        <input
          v-model="addTarget"
          class="field"
          style="max-width: 240px"
          :placeholder="t('settings.edge.addPlaceholder')"
          @keydown.enter="addTargetValue()"
        />
        <button class="btn" :disabled="!addTarget.trim()" @click="addTargetValue()">
          {{ t('settings.edge.addButton') }}
        </button>
      </div>
    </div>

    <p class="row-hint">{{ t('settings.edge.hint') }}</p>
  </section>
</template>

<style scoped>
.edge-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-top: 6px;
}
.edge-refresh {
  margin-left: auto;
}
.edge-bridge {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: transparent;
  color: var(--text);
  font-size: 13px;
  cursor: pointer;
  text-align: left;
  transition: border-color 0.12s ease, background 0.12s ease;
}
.edge-bridge:hover {
  border-color: var(--focus);
  background: var(--surface-2);
}
.edge-bridge.current {
  border-color: var(--focus);
  box-shadow: inset 2px 0 0 var(--focus);
}
.edge-dot {
  flex: 0 0 auto;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted);
}
.edge-dot.live {
  background: var(--ok);
}
.edge-id {
  font-family: var(--font-mono);
  font-size: 12px;
  flex: 1 1 auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.edge-state {
  flex: 0 0 auto;
  font-size: 12px;
  color: var(--faint);
}
.edge-current {
  flex: 0 0 auto;
  font-size: 11px;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--focus);
  color: var(--focus);
}
.edge-add {
  margin-top: 12px;
}
.edge-add-controls {
  display: flex;
  gap: 8px;
  align-items: center;
}
</style>
