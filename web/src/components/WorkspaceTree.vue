<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import TreeNode from './TreeNode.vue'
import { useWorkspaceStore } from '../stores/workspace'

const { t } = useI18n()
const store = useWorkspaceStore()
</script>

<template>
  <TreeNode v-if="store.tree" :node="store.tree" :depth="0" />
  <div v-else class="workspace-empty">{{ t('sidebar.workspaceNotConnected') }}</div>
</template>
