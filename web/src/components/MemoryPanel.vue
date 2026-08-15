<script setup lang="ts">
import { computed, ref } from 'vue'
import { Brain, Plus, Search, Trash2, X } from 'lucide-vue-next'
import { useMemoryStore } from '../stores/memory'
import type { MemoryItem } from '../types'

const memory = useMemoryStore()

const layerLabels: Record<string, string> = {
  L1_episodic: '经历',
  L2_semantic: '事实',
  L3_procedural: '剧本',
  L4_associative: '关联',
  L5_mental: '洞察',
  L6_predictive: '预判',
}
const layerOrder = ['L5_mental', 'L6_predictive', 'L2_semantic', 'L3_procedural', 'L1_episodic']

function label(m: MemoryItem): string {
  if (m.layer === 'L2_semantic' && (m.type === 'code_symbol' || m.type === 'code_file')) return '代码'
  return layerLabels[m.layer] ?? m.layer
}

const statsChips = computed(() => {
  const s = memory.stats
  if (!s) return []
  return layerOrder
    .filter((l) => (s.layers[l] ?? 0) > 0)
    .map((l) => ({ layer: l, label: layerLabels[l] ?? l, count: s.layers[l] }))
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
  return new Date(ts * 1000).toLocaleString('zh-CN', {
    month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

async function removeItem(m: MemoryItem) {
  if (!window.confirm('忘记这条记忆？')) return
  try {
    await memory.remove(m.id)
  } catch {
    // remove() 的错误已在 store 中写入 error
  }
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
          工作区记忆
          <span class="mem-ws">{{ memory.workspaceName || '默认工作区' }}</span>
        </span>
        <button class="icon-btn" @click="memory.close()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body memory-body">
        <p class="mem-tagline">woca 会记住这个工作区的决策、偏好与代码知识——越用越懂你的项目。</p>

        <div class="mem-toolbar">
          <div class="mem-search">
            <Search :size="13" />
            <input v-model="memory.query" placeholder="搜索记忆…" @input="memory.searchDebounced()" />
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
        <p v-if="memory.engine === 'none'" class="mem-empty">记忆引擎未启用（设置 → 记忆）。</p>
        <p v-else-if="!memory.loading && memory.items.length === 0" class="mem-empty">
          还没有记忆。和 woca 聊几句，它会自动记住重要的东西。
        </p>
        <p v-if="memory.loading" class="mem-empty">加载中…</p>

        <div v-if="insightItems.length" class="mem-group">
          <div class="mem-group-title">洞察与预判</div>
          <div v-for="m in insightItems" :key="m.id" class="mem-item insight" :class="m.layer">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" title="忘记" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <div v-if="normalItems.length" class="mem-group">
          <div class="mem-group-title">记忆</div>
          <div v-for="m in normalItems" :key="m.id" class="mem-item">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" title="忘记" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <button v-if="memory.hasMore() && !memory.loading" class="mem-more" @click="memory.loadMore()">
          加载更多
        </button>

        <div class="mem-add">
          <textarea
            v-model="newContent"
            rows="2"
            placeholder="手动记住一件事，例如：构建统一走 make build"
          ></textarea>
          <div class="mem-add-row">
            <input v-model="newTags" class="mem-tags-input" placeholder="标签（可选，逗号分隔）" />
            <button class="mem-add-btn" :disabled="!newContent.trim() || adding" @click="addMemory()">
              <Plus :size="13" /> 记住
            </button>
          </div>
        </div>

        <p class="mem-privacy">所有记忆只保存在本地（~/.woca/ladym.db），随时查看、编辑或删除。</p>
      </div>
    </div>
  </div>
</template>
