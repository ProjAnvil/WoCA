<script setup lang="ts">
import { PanelRightClose, RefreshCw } from 'lucide-vue-next'
import WorkspaceTree from './WorkspaceTree.vue'
import FilePreview from './FilePreview.vue'
import { useWorkspaceStore } from '../stores/workspace'

const store = useWorkspaceStore()
</script>

<template>
  <aside
    class="right-sidebar"
    :class="{ open: store.panelOpen }"
    :aria-hidden="store.panelOpen ? undefined : 'true'"
  >
    <div class="right-sidebar-inner">
      <header class="rs-header">
        <div class="rs-title-wrap">
          <span class="rs-title">工作区</span>
          <span v-if="store.tree" class="rs-sub">{{ store.tree.name }}</span>
        </div>
        <div class="rs-actions">
          <button class="icon-btn" title="刷新" @click="store.load()">
            <RefreshCw :size="15" />
          </button>
          <button class="icon-btn" title="收起" @click="store.closePanel()">
            <PanelRightClose :size="16" />
          </button>
        </div>
      </header>

      <div class="rs-body">
        <FilePreview v-if="store.preview || store.previewLoading || store.previewError" />
        <WorkspaceTree v-else />
      </div>
    </div>
  </aside>
</template>
