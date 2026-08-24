<script setup lang="ts">
import { computed, onMounted, provide, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { ShieldCheck } from 'lucide-vue-next'
import type { OpsContext } from './inject'
import UsersSection from './components/UsersSection.vue'
import SessionsSection from './components/SessionsSection.vue'
import BridgesSection from './components/BridgesSection.vue'
import AuditSection from './components/AuditSection.vue'

const { t, locale } = useI18n()

// --- shared state (module scope survives section switches) ---
const username = ref('')
const password = ref('')
const authed = ref(false)
const authError = ref('')

async function api(path: string, init?: RequestInit): Promise<Response> {
  // The session cookie carries the console identity; no credential
  // material rides the API calls themselves. An expired session flips
  // the app back to the login form instead of a dead end.
  const resp = await fetch(path, {
    ...init,
    ...(init?.body ? { headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) } } : {}),
  })
  if (resp.status === 401) {
    authed.value = false
  }
  return resp
}

// probe: an unconfigured console (or a live session) loads straight in;
// only a configured gate shows the login form.
async function probe() {
  try {
    const resp = await api('/api/gw/settings')
    if (resp.status === 401) {
      authed.value = false
      return
    }
    authed.value = true
  } catch {
    authed.value = false
  }
}

async function connect() {
  authError.value = ''
  try {
    const resp = await fetch('/api/console/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: username.value, password: password.value }),
    })
    if (resp.status === 401) {
      authError.value = t('login.invalid')
      return
    }
    if (resp.status === 400) {
      // No credentials configured server-side: the console is open.
      await probe()
      return
    }
    if (!resp.ok) throw new Error(`${resp.status}`)
    password.value = ''
    await probe()
  } catch (e) {
    authError.value = e instanceof Error ? e.message : String(e)
  }
}

async function logout() {
  await api('/api/console/logout', { method: 'POST' })
  authed.value = false
}

const sections = computed(() => [
  { id: 'users', label: t('nav.users') },
  { id: 'sessions', label: t('nav.sessions') },
  { id: 'bridges', label: t('nav.bridges') },
  { id: 'audit', label: t('nav.audit') },
])
const active = ref('users')

function toggleLocale() {
  const next = locale.value === 'zh-CN' ? 'en' : 'zh-CN'
  locale.value = next
  localStorage.setItem('woca.opsconsole.locale', next)
}

provide('ops', reactive({ api }) as unknown as OpsContext)

onMounted(probe)
</script>

<template>
  <div class="app">
    <header class="head">
      <div>
        <div class="title"><ShieldCheck :size="16" style="vertical-align: -3px" /> {{ t('title') }}</div>
        <div v-if="t('subtitle')" class="subtitle">{{ t('subtitle') }}</div>
      </div>
      <div class="head-actions">
        <button class="ghost" @click="toggleLocale">{{ locale === 'zh-CN' ? 'EN' : '中文' }}</button>
        <button v-if="authed" class="ghost" @click="logout">{{ t('login.logout') }}</button>
      </div>
    </header>

    <div v-if="!authed" class="login card">
      <div class="login-title">{{ t('login.title') }}</div>
      <input
        v-model="username"
        class="field wide"
        :placeholder="t('login.username')"
        autocomplete="username"
        @keyup.enter="connect"
      />
      <input
        v-model="password"
        type="password"
        class="field wide"
        :placeholder="t('login.password')"
        autocomplete="current-password"
        @keyup.enter="connect"
      />
      <div class="hint">{{ t('login.hint') }}</div>
      <div v-if="authError" class="err">{{ authError }}</div>
      <button class="primary" @click="connect">{{ t('login.connect') }}</button>
    </div>

    <div v-else class="body">
      <nav class="nav">
        <button
          v-for="sec in sections"
          :key="sec.id"
          class="nav-item"
          :class="{ active: active === sec.id }"
          @click="active = sec.id"
        >
          {{ sec.label }}
        </button>
      </nav>
      <main class="content">
        <UsersSection v-if="active === 'users'" />
        <SessionsSection v-if="active === 'sessions'" />
        <BridgesSection v-if="active === 'bridges'" />
        <AuditSection v-if="active === 'audit'" />
      </main>
    </div>
  </div>
</template>

<style scoped>
.app { max-width: 880px; margin: 0 auto; padding: 24px 16px; font: 14px/1.5 system-ui, -apple-system, sans-serif; color: #1f2933; }
.head { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 20px; }
.head-actions { display: flex; gap: 8px; }
.title { font-size: 18px; font-weight: 600; }
.subtitle { color: #616e7c; font-size: 12px; margin-top: 2px; }
.card { background: #fff; border: 1px solid #e4e7eb; border-radius: 10px; padding: 20px; }
.login { max-width: 420px; margin: 60px auto; display: flex; flex-direction: column; gap: 10px; }
.login-title { font-weight: 600; }
.field { padding: 7px 10px; border: 1px solid #cbd2d9; border-radius: 6px; font-size: 13px; }
.wide { width: 100%; box-sizing: border-box; }
.hint { color: #616e7c; font-size: 12px; }
.err { color: #c0392b; font-size: 12px; }
.primary { align-self: flex-start; background: #2563eb; color: #fff; border: 0; border-radius: 6px; padding: 7px 16px; cursor: pointer; }
.primary:disabled { opacity: 0.6; }
.ghost { background: none; border: 1px solid #cbd2d9; border-radius: 6px; padding: 4px 10px; cursor: pointer; }
.body { display: flex; gap: 16px; align-items: flex-start; }
.nav { display: flex; flex-direction: column; gap: 4px; min-width: 130px; }
.nav-item { text-align: left; background: none; border: 1px solid transparent; border-radius: 6px; padding: 7px 10px; cursor: pointer; color: #1f2933; }
.nav-item.active { background: #eff6ff; border-color: #bfdbfe; color: #1d4ed8; }
.content { flex: 1; display: flex; flex-direction: column; gap: 14px; }
</style>
