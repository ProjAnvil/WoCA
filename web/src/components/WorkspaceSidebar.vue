<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { FileText, PanelRightClose, RefreshCw } from 'lucide-vue-next'
import WorkspaceTree from './WorkspaceTree.vue'
import FilePreview from './FilePreview.vue'
import { useWorkspaceStore } from '../stores/workspace'

const { t } = useI18n()
const store = useWorkspaceStore()
</script>

<template>
  <aside
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

      <div class="rs-body">
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
