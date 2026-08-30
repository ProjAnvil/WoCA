import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import i18n from './i18n'
import { setUnauthorizedHandler } from './api/transport'
import { useAuthStore } from './stores/auth'
import './style.css'

const app = createApp(App).use(createPinia()).use(i18n)

// Any API 401 (not just the boot probe) raises the login gate: a session
// that died mid-app must not leave the user sending messages into a
// toast-wall of "unauthorized".
setUnauthorizedHandler((code) => useAuthStore().raise(code))

app.mount('#app')
