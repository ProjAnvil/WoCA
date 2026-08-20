import { createI18n } from 'vue-i18n'
import { en, zhCN } from './locales'
import { readPreference } from '../native/preferences'

export type Locale = 'zh-CN' | 'en'
// 'auto' follows the browser language; persistence is selected by the native
// preference adapter without changing the browser storage key.
export type LocaleOption = 'auto' | Locale
export const LOCALE_STORAGE_KEY = 'woca.pref.locale'

export function resolveLocale(pref: string | null | undefined): Locale {
  if (pref === 'zh-CN' || pref === 'en') return pref
  return (navigator.language || '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en'
}

const initialPreference = resolveInitialPref()

const i18n = createI18n({
  legacy: false,
  globalInjection: true,
  locale: resolveLocale(initialPreference === 'auto' ? null : initialPreference),
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zhCN, en },
})

// Applies a stored preference and keeps <html lang> in sync.
export function applyLocale(pref: LocaleOption) {
  const locale = resolveLocale(pref === 'auto' ? null : pref)
  i18n.global.locale.value = locale
  document.documentElement.lang = locale
}

applyLocale(initialPreference)

export function applyStoredLocalePreference() {
  applyLocale(resolveInitialPref())
}

function resolveInitialPref(): LocaleOption {
  const saved = readPreference(LOCALE_STORAGE_KEY)
  return saved === 'zh-CN' || saved === 'en' || saved === 'auto' ? saved : 'auto'
}

export default i18n
