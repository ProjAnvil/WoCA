<script setup lang="ts">
import WSkeleton from './ui/WSkeleton.vue'
import { toErrorMessage } from '../lib/errors'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ArrowUp, Check, FolderPlus, RefreshCw } from 'lucide-vue-next'
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
        <div v-if="loading" class="fp-skeleton" role="status" aria-busy="true">
          <span class="sr-only">{{ t('common.loading') }}</span>
          <div
            v-for="i in 6"
            :key="i"
            class="fp-skel-row"
            :style="{ marginLeft: `${(i % 3) * 14}px` }"
          >
            <WSkeleton variant="text" :width="`${38 + ((i * 41) % 46)}%`" height="14px" />
          </div>
        </div>
        <div v-else-if="error" class="fp-empty fp-error">{{ error }}</div>
        <div v-else-if="!listing?.dirs.length" class="fp-empty">{{ t('fs.empty') }}</div>
        <div v-for="d in listing?.dirs ?? []" :key="d.path" class="fp-row-wrap">
          <button class="fp-row" type="button" @click="load(d.path)" @dblclick="choose(d.path)">
            <span class="fp-name">📁 {{ d.name }}</span>
          </button>
          <!-- Sibling, not child: .fp-row is itself a button and nested
               buttons are invalid HTML. A real button makes the pick
               keyboard-focusable and announced (fs.pick), revealed on row
               hover AND focus-within (w320 audit — it used to be an
               opacity-0 SVG reachable by mouse hover only). -->
          <button
            class="fp-pick"
            type="button"
            :title="t('fs.pick')"
            :aria-label="t('fs.pick')"
            @click="choose(d.path)"
          >
            <Check :size="15" aria-hidden="true" />
          </button>
        </div>
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
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--panel);
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
  color: var(--brand);
  cursor: pointer;
  padding: 0;
  font: inherit;
}
.fp-crumb-current { opacity: 0.8; }
.fp-body {
  min-height: 180px;
  max-height: 40vh;
  overflow-y: auto;
  border: 1px solid var(--border);
  border-radius: 8px;
}
.fp-empty {
  display: flex;
  justify-content: center;
  padding: 40px 0;
  opacity: 0.6;
  font-size: 13px;
}
.fp-error { color: var(--danger); opacity: 1; }
.fp-row-wrap {
  position: relative;
}
.fp-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  /* Clearance for the overlapping pick button so names ellipsize first. */
  width: 100%;
  padding: 7px 38px 7px 10px;
  background: none;
  border: none;
  color: inherit;
  cursor: pointer;
  font: inherit;
  text-align: left;
}
.fp-row:hover { background: rgba(255, 255, 255, 0.05); }
.fp-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
/* Pick affordance: 24px hit target, revealed on row hover or keyboard
   focus inside the row wrapper (focus-within covers the pick itself). */
.fp-pick {
  position: absolute;
  top: 50%;
  right: 7px;
  transform: translateY(-50%);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
  opacity: 0;
}
.fp-row-wrap:hover .fp-pick,
.fp-row-wrap:focus-within .fp-pick {
  opacity: 0.8;
}
.fp-pick:hover,
.fp-pick:focus-visible {
  opacity: 1;
}
.fp-pick:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.fp-mkdir, .fp-manual {
  display: flex;
  gap: 8px;
  align-items: center;
}
.fp-mkdir input, .fp-manual input {
  flex: 1;
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 6px 8px;
  color: inherit;
  font: inherit;
}</style>
