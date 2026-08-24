import { defineStore } from 'pinia'
import { reactive } from 'vue'
import {
  getClaudeEnv,
  getManagedDelivery,
  getPlugins,
  installPlugin,
  putClaudeEnv,
  putPlugins,
  type ManagedDelivery,
} from '../api/client'
import i18n from '../i18n'
import { toErrorMessage } from '../lib/errors'
import type { ClaudeEnvState, PluginStatus } from '../types'

// The settings panel's section state (claudeenv / plugins / managed).
// These live in Pinia rather than module-scope singletons so tests get
// per-instance isolation (a fresh createPinia per mount), the state shows
// up in Vue DevTools, and the panel-close reset is a store action instead
// of a hand-maintained wiring list. Lifetime semantics are unchanged:
// state survives section switches while the panel is open and resets when
// the panel closes (SettingsPanel's unmount hook).

export const useClaudeEnvStore = defineStore('settings/claudeEnv', () => {
  const state = reactive({
    loaded: false,
    loading: false,
    saving: false,
    error: '',
    message: '',
    env: null as ClaudeEnvState | null,
  })

  async function load() {
    if (state.loading) return
    state.loading = true
    state.error = ''
    try {
      state.env = await getClaudeEnv()
      state.loaded = true
    } catch (e) {
      state.error = toErrorMessage(e)
    } finally {
      state.loading = false
    }
  }

  async function save() {
    if (!state.env) return
    state.saving = true
    state.error = ''
    state.message = ''
    try {
      await putClaudeEnv({ skills: state.env.selected.skills, mcp: state.env.selected.mcp })
      state.message = i18n.global.t('settings.claudeenv.saved')
      await load()
    } catch (e) {
      state.error = toErrorMessage(e)
    } finally {
      state.saving = false
    }
  }

  function toggleSkill(name: string) {
    const s = state.env?.selected
    if (!s) return
    s.skills = s.skills.includes(name) ? s.skills.filter((x) => x !== name) : [...s.skills, name]
  }

  function toggleMCP(name: string) {
    const s = state.env?.selected
    if (!s) return
    s.mcp = s.mcp.includes(name) ? s.mcp.filter((x) => x !== name) : [...s.mcp, name]
  }

  function reset() {
    state.loaded = false
    state.loading = false
    state.saving = false
    state.error = ''
    state.message = ''
    state.env = null
  }

  return { state, load, save, toggleSkill, toggleMCP, reset }
})

export const usePluginsStore = defineStore('settings/plugins', () => {
  const state = reactive({
    loaded: false,
    loading: false,
    error: '',
    message: '',
    plugins: [] as PluginStatus[],
    toggling: '',
  })
  // Install-progress polling: only armed while some runtime is installing.
  // Store-owned (not component-owned) because the state must keep updating
  // across section switches; reset() tears it down with the rest.
  let timer: ReturnType<typeof setInterval> | null = null

  function scheduleInstallPolling() {
    const installing = state.plugins.some((p) => p.state === 'installing')
    if (installing && timer == null) {
      timer = setInterval(() => void load(), 2000)
    } else if (!installing && timer != null) {
      clearInterval(timer)
      timer = null
    }
  }

  async function load() {
    if (state.loading) return
    state.loading = true
    state.error = ''
    try {
      const res = await getPlugins()
      state.plugins = res.plugins
      state.loaded = true
      scheduleInstallPolling()
    } catch (e) {
      state.error = toErrorMessage(e)
    } finally {
      state.loading = false
    }
  }

  async function toggle(p: PluginStatus) {
    if (state.toggling) return
    const enable = !p.enabled
    state.toggling = p.name
    state.error = ''
    state.message = ''
    const names = state.plugins
      .filter((x) => (x.name === p.name ? enable : x.enabled))
      .map((x) => x.name)
    try {
      await putPlugins(names)
      p.enabled = enable
      state.message = enable
        ? i18n.global.t('settings.plugins.enabled', {
            name: p.name,
            note:
              p.state !== 'ready'
                ? i18n.global.t('settings.plugins.enabledRuntimeMissing')
                : i18n.global.t('settings.plugins.toolsInstant'),
          })
        : i18n.global.t('settings.plugins.disabled', { name: p.name })
    } catch (e) {
      state.error = toErrorMessage(e)
    } finally {
      state.toggling = ''
    }
  }

  async function install(p: PluginStatus, reinstall: boolean) {
    state.error = ''
    state.message = ''
    try {
      await installPlugin(p.name, reinstall)
      p.state = 'installing'
      p.log = []
      state.message = reinstall
        ? i18n.global.t('settings.plugins.upgradeStarted', { name: p.name })
        : i18n.global.t('settings.plugins.installStarted', { name: p.name })
      scheduleInstallPolling()
    } catch (e) {
      state.error = toErrorMessage(e)
    }
  }

  function reset() {
    if (timer != null) {
      clearInterval(timer)
      timer = null
    }
    state.loaded = false
    state.loading = false
    state.error = ''
    state.message = ''
    state.plugins = []
    state.toggling = ''
  }

  return { state, load, toggle, install, reset }
})

export const useManagedStore = defineStore('settings/managed', () => {
  const state = reactive({
    loaded: false,
    loading: false,
    error: '',
    delivery: null as ManagedDelivery | null,
  })

  async function load() {
    if (state.loading) return
    state.loading = true
    state.error = ''
    try {
      state.delivery = await getManagedDelivery()
      state.loaded = true
    } catch (e) {
      state.error = toErrorMessage(e)
    } finally {
      state.loading = false
    }
  }

  function reset() {
    state.loaded = false
    state.loading = false
    state.error = ''
    state.delivery = null
  }

  return { state, load, reset }
})
