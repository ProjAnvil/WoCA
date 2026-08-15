<script setup lang="ts">
import TreeNode from './TreeNode.vue'
import { useWorkspaceStore } from '../stores/workspace'

const store = useWorkspaceStore()
</script>

<template>
  <TreeNode v-if="store.tree" :node="store.tree" :depth="0" />
  <div v-else class="workspace-empty">工作区尚未接入</div>
</template>
