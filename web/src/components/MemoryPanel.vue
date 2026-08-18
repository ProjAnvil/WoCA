<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Brain, Plus, Search, Trash2, X } from 'lucide-vue-next'
import { useMemoryStore } from '../stores/memory'
import type { MemoryItem } from '../types'

const { t, locale } = useI18n()
const memory = useMemoryStore()

// Layer labels carry the L1~L6 number (e.g. "L2 facts") so the chip, the badge
// and the layer ordering all read consistently.
const layerLabelKeys: Record<string, string> = {
  L1_episodic: 'episodic',
  L2_semantic: 'semantic',
  L3_procedural: 'procedural',
  L4_associative: 'associative',
  L5_mental: 'mental',
  L6_predictive: 'predictive',
}

function layerLabel(layer: string): string {
  const key = layerLabelKeys[layer]
  return key ? t(`memory.layer.${key}`) : layer
}

const layerOrder = ['L5_mental', 'L6_predictive', 'L2_semantic', 'L3_procedural', 'L1_episodic']

function label(m: MemoryItem): string {
  if (m.layer === 'L2_semantic' && (m.type === 'code_symbol' || m.type === 'code_file')) return t('memory.layer.code')
  return layerLabel(m.layer)
}

const statsChips = computed(() => {
  const s = memory.stats
  if (!s) return []
  return layerOrder
    .filter((l) => (s.layers[l] ?? 0) > 0)
    .map((l) => ({ layer: l, label: layerLabel(l), count: s.layers[l] }))
})

// Insights & predictions render as their own group at the top.
const insightItems = computed(() =>
  memory.items.filter((m) => m.layer === 'L5_mental' || m.layer === 'L6_predictive'),
)
const normalItems = computed(() =>
  memory.items.filter((m) => m.layer !== 'L5_mental' && m.layer !== 'L6_predictive'),
)

const newContent = ref('')
const newTags = ref('')
const adding = ref(false)

function fmtTime(ts: number): string {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleString(locale.value === 'en' ? 'en' : 'zh-CN', {
    month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

async function removeItem(m: MemoryItem) {
  if (!window.confirm(t('memory.forgetConfirm'))) return
  // remove() catches errors internally and writes them to the store's error,
  // surfaced by the mem-error row.
  await memory.remove(m.id)
}

async function addMemory() {
  const content = newContent.value.trim()
  if (!content || adding.value) return
  adding.value = true
  try {
    const tags = newTags.value.split(/[,，\s]+/).filter(Boolean)
    await memory.add(content, tags)
    newContent.value = ''
    newTags.value = ''
  } finally {
    adding.value = false
  }
}

function toggleLayer(layer: string) {
  memory.layerFilter = memory.layerFilter === layer ? '' : layer
  void memory.reload()
}
</script>

<template>
  <div class="overlay" @click.self="memory.close()">
    <div class="card memory-card">
      <div class="card-head">
        <span class="card-title">
          <Brain :size="15" />
          {{ t('memory.title') }}
          <span class="mem-ws">{{ memory.workspaceName || t('memory.defaultWorkspace') }}</span>
        </span>
        <button class="icon-btn" @click="memory.close()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body memory-body">
        <p class="mem-tagline">{{ t('memory.tagline') }}</p>

        <div class="mem-toolbar">
          <div class="mem-search">
            <Search :size="13" />
            <input v-model="memory.query" :placeholder="t('memory.searchPlaceholder')" @input="memory.searchDebounced()" />
          </div>
          <div v-if="statsChips.length" class="mem-chips">
            <button
              v-for="c in statsChips"
              :key="c.layer"
              class="mem-chip"
              :class="{ active: memory.layerFilter === c.layer }"
              @click="toggleLayer(c.layer)"
            >
              {{ c.label }} {{ c.count }}
            </button>
          </div>
        </div>

        <p v-if="memory.error" class="mem-error">{{ memory.error }}</p>
        <p v-if="memory.notice" class="mem-notice">{{ memory.notice }}</p>
        <p v-if="memory.engine === 'none'" class="mem-empty">{{ t('memory.engineDisabled') }}</p>
        <p v-else-if="!memory.loading && memory.items.length === 0" class="mem-empty">
          {{ t('memory.empty') }}
        </p>
        <p v-if="memory.loading" class="mem-empty">{{ t('common.loading') }}</p>

        <div v-if="insightItems.length" class="mem-group">
          <div class="mem-group-title">{{ t('memory.insights') }}</div>
          <div v-for="m in insightItems" :key="m.id" class="mem-item insight" :class="m.layer">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" :title="t('memory.forget')" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <div v-if="normalItems.length" class="mem-group">
          <div class="mem-group-title">{{ t('memory.memories') }}</div>
          <div v-for="m in normalItems" :key="m.id" class="mem-item">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" :title="t('memory.forget')" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <button v-if="memory.hasMore() && !memory.loading" class="mem-more" @click="memory.loadMore()">
          {{ t('memory.loadMore') }}
        </button>

        <div v-if="memory.engine !== 'none'" class="mem-add">
          <textarea
            v-model="newContent"
            rows="2"
            :placeholder="t('memory.addPlaceholder')"
          ></textarea>
          <div class="mem-add-row">
            <input v-model="newTags" class="mem-tags-input" :placeholder="t('memory.tagsPlaceholder')" />
            <button class="mem-add-btn" :disabled="!newContent.trim() || adding" @click="addMemory()">
              <Plus :size="13" /> {{ t('memory.remember') }}
            </button>
          </div>
        </div>

        <p class="mem-privacy">{{ t('memory.privacy') }}</p>
      </div>
    </div>
  </div>
</template>
