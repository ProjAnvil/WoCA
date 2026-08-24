<script setup lang="ts">
import { toErrorMessage } from '../lib/errors'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowUp, Check, FolderPlus, Loader2, RefreshCw } from 'lucide-vue-next'
import { fsList, fsMkdir, type FsListing } from '../api/client'

// The workspace folder browser (product design §6.4): directories only,
// server-confirmed mkdir, breadcrumb drill-down. Manual paths stay
// available via the dialog's path input in the parent component.
const { t } = useI18n()
const emit = defineEmits<{ close: []; select: [path: string] }>()

const listing = ref<FsListing | null>(null)
const loading = ref(false)
const error = ref('')
const creating = ref(false)
const newDirName = ref('')
const manualPath = ref('')

async function load(path?: string) {
  loading.value = true
  error.value = ''
  try {
    listing.value = await fsList(path)
  } catch (e) {
    error.value = toErrorMessage(e)
  } finally {
    loading.value = false
  }
}

onMounted(() => load())

async function mkdir() {
  const name = newDirName.value.trim()
  if (!name || !listing.value?.path) return
  try {
    await fsMkdir(listing.value.path, name)
    newDirName.value = ''
    await load(listing.value.path)
  } catch (e) {
    error.value = toErrorMessage(e)
  }
}

function choose(path: string) {
  emit('select', path)
}
</script>

<template>
  <div class="modal-backdrop" @click.self="emit('close')">
    <div class="folder-picker">
      <div class="fp-head">
        <span>{{ t('fs.title') }}</span>
        <div class="fp-actions">
          <button
            v-if="listing?.parent !== undefined && listing.parent"
            class="icon-btn"
            :title="t('fs.up')"
            @click="load(listing.parent)"
          >
            <ArrowUp :size="15" />
          </button>
          <button class="icon-btn" :title="t('fs.refresh')" @click="load(listing?.path || undefined)">
            <RefreshCw :size="15" />
          </button>
        </div>
      </div>

      <div v-if="listing" class="fp-breadcrumb">
        <template v-for="root in listing.roots" :key="root">
          <button v-if="listing.path === root || listing.path === ''" class="fp-crumb" @click="load(root)">
            {{ root }}
          </button>
        </template>
        <span v-if="listing.path && !listing.roots.includes(listing.path)" class="fp-crumb-current">
          {{ listing.path }}
        </span>
      </div>

      <div class="fp-body">
        <div v-if="loading" class="fp-empty"><Loader2 class="spin" :size="18" /></div>
        <div v-else-if="error" class="fp-empty fp-error">{{ error }}</div>
        <div v-else-if="!listing?.dirs.length" class="fp-empty">{{ t('fs.empty') }}</div>
        <button
          v-for="d in listing?.dirs ?? []"
          :key="d.path"
          class="fp-row"
          @click="load(d.path)"
          @dblclick="choose(d.path)"
        >
          <span class="fp-name">📁 {{ d.name }}</span>
          <Check :size="15" class="fp-pick" :title="t('fs.pick')" @click.stop="choose(d.path)" />
        </button>
      </div>

      <div class="fp-mkdir">
        <FolderPlus :size="15" />
        <input
          v-model="newDirName"
          :placeholder="t('fs.newDir')"
          @keydown.enter.stop.prevent="mkdir"
        />
        <button class="btn" :disabled="!newDirName.trim() || creating" @click="mkdir">
          {{ t('fs.create') }}
        </button>
      </div>

      <div class="fp-manual">
        <input v-model="manualPath" :placeholder="t('fs.manualPath')" @keydown.enter.stop.prevent="choose(manualPath.trim())" />
        <button class="btn btn-primary" :disabled="!manualPath.trim()" @click="choose(manualPath.trim())">
          {{ t('fs.use') }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.folder-picker {
  width: min(560px, 92vw);
  max-height: 76vh;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 14px;
  background: var(--panel, #1b1e24);
  padding: 14px;
  gap: 10px;
}
.fp-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.fp-actions { display: flex; gap: 4px; }
.fp-breadcrumb {
  display: flex;
  gap: 6px;
  font-size: 12px;
  opacity: 0.85;
  flex-wrap: wrap;
}
.fp-crumb {
  background: none;
  border: none;
  color: var(--accent, #4f7cff);
  cursor: pointer;
  padding: 0;
  font: inherit;
}
.fp-crumb-current { opacity: 0.8; }
.fp-body {
  min-height: 180px;
  max-height: 40vh;
  overflow-y: auto;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 8px;
}
.fp-empty {
  display: flex;
  justify-content: center;
  padding: 40px 0;
  opacity: 0.6;
  font-size: 13px;
}
.fp-error { color: #e5534b; opacity: 1; }
.fp-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  padding: 7px 10px;
  background: none;
  border: none;
  color: inherit;
  cursor: pointer;
  font: inherit;
  text-align: left;
}
.fp-row:hover { background: rgba(255, 255, 255, 0.05); }
.fp-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fp-pick { opacity: 0; }
.fp-row:hover .fp-pick { opacity: 0.8; }
.fp-mkdir, .fp-manual {
  display: flex;
  gap: 8px;
  align-items: center;
}
.fp-mkdir input, .fp-manual input {
  flex: 1;
  background: transparent;
  border: 1px solid var(--border, #2a2e37);
  border-radius: 6px;
  padding: 6px 8px;
  color: inherit;
  font: inherit;
}</style>
