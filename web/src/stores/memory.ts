import { defineStore } from 'pinia'
import { ref } from 'vue'
import { forgetMemory, listMemory, memoryStats, rememberMemory } from '../api/client'
import type { MemoryItem, MemoryStats, RememberResponse } from '../types'

// Workspace-scoped memory panel state. open(ws) fixes the workspace for the
// panel's lifetime; reopening refreshes it.
export const useMemoryStore = defineStore('memory', () => {
  const panelOpen = ref(false)
  const workspace = ref('')
  const workspaceName = ref('')
  const items = ref<MemoryItem[]>([])
  const stats = ref<MemoryStats | null>(null)
  const query = ref('')
  const layerFilter = ref('')
  const loading = ref(false)
  const engine = ref('ladym')
  const error = ref('')
  const notice = ref('') // e.g. gate-drop feedback after 手动记住
  let offset = 0
  const pageSize = 20
  let searchTimer: ReturnType<typeof setTimeout> | null = null

  function open(ws: string, name?: string) {
    workspace.value = ws ?? ''
    workspaceName.value = name ?? ws ?? ''
    panelOpen.value = true
    query.value = ''
    layerFilter.value = ''
    notice.value = ''
    void reload()
  }

  function close() {
    panelOpen.value = false
  }

  async function reload() {
    loading.value = true
    error.value = ''
    notice.value = ''
    offset = 0
    try {
      const [list, st] = await Promise.all([
        listMemory({
          workspace: workspace.value,
          q: query.value || undefined,
          layer: layerFilter.value || undefined,
          limit: pageSize,
        }),
        memoryStats(workspace.value),
      ])
      engine.value = list.engine
      items.value = list.items
      offset = list.items.length
      stats.value = st.stats
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  function searchDebounced() {
    if (searchTimer) clearTimeout(searchTimer)
    searchTimer = setTimeout(() => void reload(), 300)
  }

  async function loadMore() {
    if (loading.value) return
    loading.value = true
    try {
      const list = await listMemory({
        workspace: workspace.value,
        q: query.value || undefined,
        layer: layerFilter.value || undefined,
        limit: pageSize,
        offset,
      })
      items.value = [...items.value, ...list.items]
      offset += list.items.length
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  function hasMore(): boolean {
    // 搜索模式下后端 Recall 忽略 offset，加载更多会重复追加第一页，因此只在浏览模式分页。
    if (query.value.trim()) return false
    return items.value.length >= pageSize && items.value.length % pageSize === 0
  }

  async function add(content: string, tags: string[] = []): Promise<string> {
    let res: RememberResponse
    try {
      res = await rememberMemory({ workspace: workspace.value, content, tags })
    } catch (e) {
      error.value = e instanceof Error ? `记住失败：${e.message}` : '记住失败，请稍后重试'
      return ''
    }
    await reload()
    // 注意：必须在 reload() 之后写入 —— reload() 开头会清空 notice，先写会被覆盖。
    if (res.dropped || !res.id) {
      notice.value = '被注意力门丢弃（重复或噪音），未保存'
    } else {
      notice.value = ''
    }
    return res.id
  }

  async function remove(id: string) {
    try {
      await forgetMemory(id)
    } catch (e) {
      error.value = e instanceof Error ? `忘记失败：${e.message}` : '忘记失败，请稍后重试'
      return
    }
    items.value = items.value.filter((m) => m.id !== id)
    void reload()
  }

  return {
    panelOpen, workspace, workspaceName, items, stats, query, layerFilter,
    loading, engine, error, notice,
    open, close, reload, searchDebounced, loadMore, hasMore, add, remove,
  }
})
