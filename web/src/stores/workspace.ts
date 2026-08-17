import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { WorkspaceFile } from '../types'
import { getWorkspace, getWorkspaceFile } from '../api/client'

export const useWorkspaceStore = defineStore('workspace', () => {
  // Populated from GET /api/workspace; the standalone demo app sets it
  // directly with mock data instead.
  const tree = ref<WorkspaceFile | null>(null)
  // The workspace path the tree was loaded for ('' = the server's startup
  // directory). Session switches reload the tree for the new workspace and
  // file previews resolve against the same root.
  const root = ref('')

  /** Whether the right-hand file browser sidebar is open. */
  const panelOpen = ref(false)
  /** The file currently shown in the preview pane (null = tree view). */
  const preview = ref<WorkspaceFile | null>(null)
  const previewLoading = ref(false)
  const previewError = ref('')

  // Demo mode (standalone demo app): files without inline content are not
  // fetched from a backend.
  const demo = ref(false)
  function setDemo(v: boolean) {
    demo.value = v
  }

  async function load(workspace?: string) {
    if (workspace !== undefined) root.value = workspace
    try {
      tree.value = await getWorkspace(root.value || undefined)
    } catch {
      tree.value = null
    }
  }

  function togglePanel() {
    panelOpen.value = !panelOpen.value
  }

  function openPanel() {
    panelOpen.value = true
  }

  function closePanel() {
    panelOpen.value = false
  }

  // Files with inline content (markdown) preview instantly; other files are
  // fetched on demand from /api/workspace/file for code-highlighted preview.
  async function openPreview(file: WorkspaceFile) {
    if (file.kind !== 'file') return
    if (file.content != null) {
      preview.value = file
      previewError.value = ''
      return
    }
    if (demo.value) {
      preview.value = file
      previewError.value = '演示模式未包含该文件的内容'
      return
    }
    previewLoading.value = true
    previewError.value = ''
    preview.value = { ...file, content: '' }
    try {
      const full = await getWorkspaceFile(file.path, root.value || undefined)
      preview.value = full
    } catch (e) {
      previewError.value = e instanceof Error ? e.message : String(e)
      preview.value = null
    } finally {
      previewLoading.value = false
    }
  }

  function closePreview() {
    preview.value = null
    previewError.value = ''
  }

  return {
    tree,
    root,
    panelOpen,
    preview,
    previewLoading,
    previewError,
    load,
    togglePanel,
    openPanel,
    closePanel,
    openPreview,
    closePreview,
    setDemo,
  }
})
