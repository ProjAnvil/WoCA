import { createI18n } from 'vue-i18n'
import { en, zhCN } from './locales'

export type Locale = 'zh-CN' | 'en'
// 'auto' follows the browser language; persisted in localStorage alongside
// the other woca.pref.* client-side settings.
export type LocaleOption = 'auto' | Locale
export const LOCALE_STORAGE_KEY = 'woca.pref.locale'

export function resolveLocale(pref: string | null | undefined): Locale {
  if (pref === 'zh-CN' || pref === 'en') return pref
  return (navigator.language || '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en'
}

const i18n = createI18n({
  legacy: false,
  globalInjection: true,
  locale: resolveLocale(localStorage.getItem(LOCALE_STORAGE_KEY)),
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zhCN, en },
})

// Applies a stored preference and keeps <html lang> in sync.
export function applyLocale(pref: LocaleOption) {
  const locale = resolveLocale(pref === 'auto' ? null : pref)
  i18n.global.locale.value = locale
  document.documentElement.lang = locale
}

applyLocale(resolveInitialPref())

function resolveInitialPref(): LocaleOption {
  const saved = localStorage.getItem(LOCALE_STORAGE_KEY)
  return saved === 'zh-CN' || saved === 'en' || saved === 'auto' ? saved : 'auto'
}

export default i18n
