<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronRight, FileText } from 'lucide-vue-next'
import type { WorkspaceFile } from '../types'
import { useWorkspaceStore } from '../stores/workspace'

defineOptions({ name: 'TreeNode' })

const props = defineProps<{ node: WorkspaceFile; depth?: number }>()
const store = useWorkspaceStore()
const expanded = ref(true)

const isDir = computed(() => props.node.kind === 'dir')
const previewable = computed(() => props.node.kind === 'file' && props.node.content != null)

function onClick() {
  if (isDir.value) {
    expanded.value = !expanded.value
  } else if (previewable.value) {
    store.openPreview(props.node)
  }
}
</script>

<template>
  <div class="tree-node">
    <button
      class="tree-row"
      :class="{ 'is-file': !isDir, 'is-previewable': previewable }"
      :style="{ paddingLeft: `${(props.depth ?? 0) * 14 + 8}px` }"
      @click="onClick"
    >
      <ChevronRight v-if="isDir" :size="14" class="chevron" :class="{ open: expanded }" />
      <FileText v-else :size="14" class="tree-icon" />
      <span class="tree-label">{{ props.node.name }}</span>
    </button>

    <template v-if="isDir && expanded">
      <TreeNode
        v-for="child in props.node.children"
        :key="child.path"
        :node="child"
        :depth="(props.depth ?? 0) + 1"
      />
    </template>
  </div>
</template>
