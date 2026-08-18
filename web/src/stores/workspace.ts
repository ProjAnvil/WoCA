import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { WorkspaceFile } from '../types'
import { getWorkspace, getWorkspaceFile } from '../api/client'
import i18n from '../i18n'

// Split-layout constants (px). The chat column keeps a usable minimum —
// bubbles, tool cards and the composer stay readable at 380px (mirrors the
// .main min-width in style.css); the left session sidebar is fixed at 280px.
export const CHAT_MIN_WIDTH = 380
const LEFT_SIDEBAR_WIDTH = 280
// The split handle is a flex sibling (7px wide, see .split-handle in
// style.css); its width must be budgeted too or the row overflows and the
// body becomes horizontally scrollable while dragging.
const SPLIT_HANDLE_WIDTH = 7
// The workspace panel hosts the file tree and the rendered file side by
// side; below this the two panes crowd each other unreadably.
const WORKSPACE_MIN_WIDTH = 420

function viewportWidth(): number {
  if (typeof window === 'undefined') return 0
  return document.documentElement.clientWidth || window.innerWidth
}

// Most the workspace panel may take: every pixel not needed to keep the
// chat at CHAT_MIN_WIDTH. Never negative.
function workspaceMaxWidth(): number {
  return Math.max(
    0,
    viewportWidth() - LEFT_SIDEBAR_WIDTH - SPLIT_HANDLE_WIDTH - CHAT_MIN_WIDTH,
  )
}

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
  /** Workspace panel width in px; bound to the CSS var --ws-w. */
  const width = ref(0)
  /** True while the split handle is being dragged (disables the open/close transition). */
  const dragging = ref(false)
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
    if (panelOpen.value) closePanel()
    else openPanel()
  }

  // Opening defaults to the widest layout: the workspace takes everything
  // except the chat's minimum width, and the user can then drag narrower.
  function openPanel() {
    resizeTo(Infinity)
    panelOpen.value = true
  }

  // Clamp a candidate panel width into [min, max]. The lower bound also
  // yields to the max on narrow viewports, where squeezing the chat below
  // its minimum is worse than a cramped workspace.
  function resizeTo(px: number) {
    const max = workspaceMaxWidth()
    const lo = Math.min(WORKSPACE_MIN_WIDTH, max)
    width.value = Math.min(Math.max(px, lo), max)
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
      previewError.value = i18n.global.t('workspace.demoNoContent')
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
    width,
    dragging,
    preview,
    previewLoading,
    previewError,
    load,
    togglePanel,
    openPanel,
    closePanel,
    resizeTo,
    openPreview,
    closePreview,
    setDemo,
  }
})
