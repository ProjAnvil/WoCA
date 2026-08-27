// Client-side preferences: theme, locale, panel visibility, and the
// browser/native-persisted preference refs. Server-backed settings live in
// stores/serverSettings.ts; the only bridge is openPanel loading the
// server snapshot when the settings panel opens.
import { defineStore } from 'pinia'
import { useToggle } from '@vueuse/core'
import { computed, ref, watch } from 'vue'
import i18n, { applyLocale, LOCALE_STORAGE_KEY, resolveLocale, type LocaleOption } from '../i18n'
import { usePreference, useThemePreference } from '../native/preferences'

export const useSettingsStore = defineStore('settings', () => {
  const isDark = useThemePreference()
  const toggleDark = useToggle(isDark)

  const panelOpen = ref(false)

  const localePref = usePreference<LocaleOption>(LOCALE_STORAGE_KEY, 'auto')
  const locale = computed(() => resolveLocale(localePref.value))
  function setLocale(pref: LocaleOption) {
    localePref.value = pref
    applyLocale(pref)
  }
  watch(localePref, (pref) => applyLocale(pref))

  // Client-side preferences (cards in the settings panel). The refs stay
  // assignable through the store while the adapter chooses browser or native
  // persistence.
  const autoCollapseTools = usePreference('woca.pref.autoCollapseTools', true)
  const markdownPreview = usePreference('woca.pref.markdownPreview', true)
  const model = usePreference('woca.pref.model', 'claude-sonnet-4-5')
  const permissionMode = usePreference<'default' | 'acceptEdits' | 'bypassPermissions' | 'readOnly' | 'plan'>(
    'woca.pref.permissionMode',
    'default',
  )

  // Information density: drives the --space-unit token via
  // html[data-density] (see styles/tokens.css).
  const density = usePreference<'comfortable' | 'compact'>('woca.pref.density', 'comfortable')
  watch(
    density,
    (d) => {
      document.documentElement.dataset.density = d
    },
    { immediate: true },
  )

  function openPanel() {
    panelOpen.value = true
    // Opening the panel refreshes the server-backed snapshot (lazy import
    // avoids a store-init cycle: serverSettings imports this store).
    void import('./serverSettings').then(({ useServerSettingsStore }) => useServerSettingsStore().loadServer())
  }
  function closePanel() {
    panelOpen.value = false
  }

  return {
    isDark,
    toggleDark,
    panelOpen,
    localePref,
    locale,
    setLocale,
    autoCollapseTools,
    markdownPreview,
    model,
    permissionMode,
    density,
    openPanel,
    closePanel,
  }
})
