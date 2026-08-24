import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { getStatus, type BridgeStatus } from '../api/client'

// The bridge status probe (/api/status), shared by the sidebar health lamp,
// the settings panel's edition gating, and the status modal. One store
// replaces three independent fetch sites (and their divergent error
// handling): whoever mounts first populates it, the sidebar's poll keeps
// it fresh, and everyone reads the same snapshot.
export const useStatusStore = defineStore('status', () => {
  const status = ref<BridgeStatus | null>(null)
  const failed = ref(false)

  async function refresh() {
    try {
      status.value = await getStatus()
      failed.value = false
    } catch {
      failed.value = true
    }
  }

  // Global health lamp (product design §6.3): green = synced, yellow =
  // STALE cache, grey = standalone, red = the probe itself failed (the
  // pre-login state; the login gate surfaces the reason).
  const lamp = computed<'ok' | 'stale' | 'off' | 'err'>(() => {
    if (failed.value || !status.value) return 'err'
    if (!status.value.cloud?.enrolled) return 'off'
    return status.value.cloud.stale ? 'stale' : 'ok'
  })

  return { status, failed, refresh, lamp }
})
