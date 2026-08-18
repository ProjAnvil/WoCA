<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronRight } from 'lucide-vue-next'
import type { WorkspaceFile } from '../types'
import { useWorkspaceStore } from '../stores/workspace'
import { isTextPreviewable } from '../lib/files'
import FileIcon from './FileIcon.vue'

defineOptions({ name: 'TreeNode' })

const props = defineProps<{ node: WorkspaceFile; depth?: number }>()
const store = useWorkspaceStore()
const expanded = ref(true)

const isDir = computed(() => props.node.kind === 'dir')
// Markdown ships inline content; every other text file is fetched on demand
// by the workspace store when previewed.
const previewable = computed(
  () => props.node.kind === 'file' && (props.node.content != null || isTextPreviewable(props.node.name)),
)

function onClick() {
  if (isDir.value) {
    expanded.value = !expanded.value
  } else if (previewable.value) {
    void store.openPreview(props.node)
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
      <FileIcon v-else :name="props.node.name" :size="14" class="tree-icon" />
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
