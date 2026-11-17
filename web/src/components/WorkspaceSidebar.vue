<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { FileDiff, FileText, PanelRightClose, RefreshCw } from 'lucide-vue-next'
import WorkspaceTree from './WorkspaceTree.vue'
import FilePreview from './FilePreview.vue'
import ChangesTab from './ChangesTab.vue'
import { useWorkspaceStore } from '../stores/workspace'
import { useFocusTrap } from '../composables/useFocusTrap'

const { t } = useI18n()
const store = useWorkspaceStore()

// ≤1100px the panel turns from a static split pane into an overlay
// drawer (panels.css @media 1100) and behaves modally: focus trap +
// Esc-to-close. Wide shells keep the panel untrapped — it is layout, not
// a dialog. The query mirrors the CSS breakpoint and is tracked live so
// crossing the threshold while open re-evaluates the trap.
const panel = ref<HTMLElement | null>(null)
const drawerShell = ref(false)
const onShellChange = (e: MediaQueryListEvent) => {
  drawerShell.value = e.matches
}
let shellMq: MediaQueryList | null = null
onMounted(() => {
  if (typeof window.matchMedia !== 'function') return
  shellMq = window.matchMedia('(max-width: 1100px)')
  drawerShell.value = shellMq.matches
  shellMq.addEventListener('change', onShellChange)
})
onBeforeUnmount(() => shellMq?.removeEventListener('change', onShellChange))

useFocusTrap(panel, computed(() => store.panelOpen && drawerShell.value), {
  esc: () => store.closePanel(),
})
</script>

<template>
  <aside
    ref="panel"
    class="right-sidebar"
    :class="{ open: store.panelOpen, dragging: store.dragging }"
    :style="store.width > 0 ? { '--ws-w': `${store.width}px` } : undefined"
    :aria-hidden="store.panelOpen ? undefined : 'true'"
  >
    <div class="right-sidebar-inner">
      <header class="rs-header">
        <div class="rs-title-wrap">
          <span class="rs-title">{{ t('sidebar.workspace') }}</span>
          <span v-if="store.tree" class="rs-sub">{{ store.tree.name }}</span>
        </div>
        <div class="rs-actions">
          <button class="icon-btn" :title="t('common.refresh')" @click="store.load()">
            <RefreshCw :size="15" />
          </button>
          <button class="icon-btn" :title="t('sidebar.collapsePanel')" @click="store.closePanel()">
            <PanelRightClose :size="16" />
          </button>
        </div>
      </header>

      <div class="rs-tabs" role="tablist">
        <button
          class="rs-tab"
          role="tab"
          :aria-selected="store.tab === 'files'"
          :class="{ active: store.tab === 'files' }"
          type="button"
          @click="store.tab = 'files'"
        >
          <FileText :size="13" /> {{ t('sidebar.tabFiles') }}
        </button>
        <button
          class="rs-tab"
          role="tab"
          :aria-selected="store.tab === 'changes'"
          :class="{ active: store.tab === 'changes' }"
          type="button"
          @click="store.openChangesTab()"
        >
          <FileDiff :size="13" /> {{ t('sidebar.tabChanges') }}
          <span v-if="store.changes.length" class="rs-tab-count">{{ store.changes.length }}</span>
        </button>
      </div>

      <div v-if="store.tab === 'changes'" class="rs-body rs-body-changes">
        <ChangesTab />
      </div>
      <div v-else class="rs-body">
        <div class="ws-tree-pane">
          <WorkspaceTree />
        </div>
        <div class="ws-preview-pane">
          <FilePreview
            v-if="store.preview || store.previewLoading || store.previewError"
          />
          <div v-else class="ws-preview-empty">
            <FileText :size="18" />
            <span>{{ t('sidebar.selectFileToPreview') }}</span>
          </div>
        </div>
      </div>
    </div>
  </aside>
</template>
