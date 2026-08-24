import { createI18n } from 'vue-i18n'
import en from './locales/en'
import zhCN from './locales/zh-CN'

const saved = typeof localStorage !== 'undefined'
  ? localStorage.getItem('woca.configsvc.locale')
  : null

export default createI18n({
  legacy: false,
  locale: saved || (typeof navigator !== 'undefined' && navigator.language?.startsWith('zh') ? 'zh-CN' : 'en'),
  fallbackLocale: 'en',
  messages: { en, 'zh-CN': zhCN },
})
