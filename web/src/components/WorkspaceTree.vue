<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import TreeNode from './TreeNode.vue'
import WSkeleton from './ui/WSkeleton.vue'
import { useWorkspaceStore } from '../stores/workspace'

const { t } = useI18n()
const store = useWorkspaceStore()
</script>

<template>
  <TreeNode v-if="store.tree" :node="store.tree" :depth="0" />
  <div v-else-if="store.treeLoading" class="tree-skeleton" role="status" aria-busy="true">
    <span class="sr-only">{{ t('common.loading') }}</span>
    <div
      v-for="i in 6"
      :key="i"
      class="tree-skeleton-row"
      :style="{ marginLeft: `${(i % 3) * 12}px` }"
    >
      <WSkeleton variant="text" :width="`${42 + ((i * 37) % 48)}%`" />
    </div>
  </div>
  <div v-else class="workspace-empty">{{ t('sidebar.workspaceNotConnected') }}</div>
</template>
