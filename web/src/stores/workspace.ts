import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { WorkspaceFile } from '../types'
import { getWorkspace } from '../api/client'

export const useWorkspaceStore = defineStore('workspace', () => {
  // Populated from GET /api/workspace (the agent's workdir); the standalone
  // demo app sets it directly with mock data instead.
  const tree = ref<WorkspaceFile | null>(null)
  /** The file currently shown in the Markdown preview (null = closed). */
  const preview = ref<WorkspaceFile | null>(null)

  async function load() {
    try {
      tree.value = await getWorkspace()
    } catch {
      tree.value = null
    }
  }

  function openPreview(file: WorkspaceFile) {
    if (file.kind === 'file' && file.content != null) {
      preview.value = file
    }
  }

  function closePreview() {
    preview.value = null
  }

  return { tree, preview, load, openPreview, closePreview }
})
