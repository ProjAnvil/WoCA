<script setup lang="ts">
import { computed, onMounted, provide, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { MessageSquare, ServerCog } from 'lucide-vue-next'
import type { Settings } from './types'
import type { CfgContext } from './inject'
import ModelSection from './components/ModelSection.vue'
import StorageSection from './components/StorageSection.vue'
import MemorySection from './components/MemorySection.vue'
import MsgPlaneSection from './components/MsgPlaneSection.vue'
import OpsSection from './components/OpsSection.vue'

const { t, locale } = useI18n()

// --- shared state (module scope survives section switches) ---
const key = ref(localStorage.getItem('woca.configsvc.key') || '')
const authed = ref(false)
const authError = ref('')
const saving = ref(false)
const savedFlash = ref('')
const error = ref('')

const s = ref<Settings | null>(null)
const drafts = ref<Record<string, string>>({})

function d(field: string): string {
  return drafts.value[field] ?? ''
}
function setDraft(field: string, v: string) {
  drafts.value[field] = v
}

// Effective value: the draft when the operator touched the field, else the
// loaded snapshot. Conditional rendering binds to this so changing a
// backend/engine/provider select immediately reveals its sub-fields.
function v(field: string): string {
  if (drafts.value[field] !== undefined) return drafts.value[field]
  const cur = s.value as unknown as Record<string, unknown> | null
  const val = cur?.[field]
  return val === undefined || val === null ? '' : String(val)
}

async function api(path: string, init?: RequestInit): Promise<Response> {
  return fetch(path, {
    ...init,
    headers: {
      Authorization: `Bearer ${key.value}`,
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(init?.headers || {}),
    },
  })
}

async function load() {
  error.value = ''
  try {
    const resp = await api('/api/settings')
    if (resp.status === 401 || resp.status === 403) {
      authed.value = false
      authError.value = t('login.invalid')
      return
    }
    if (!resp.ok) throw new Error(`${resp.status}`)
    s.value = (await resp.json()) as Settings
    authed.value = true
    authError.value = ''
    drafts.value = {}
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function connect() {
  localStorage.setItem('woca.configsvc.key', key.value)
  await load()
}

// save composes the current drafts into a PUT body; only fields the
// operator actually touched are sent (absent = unchanged).
async function save(extra?: Record<string, unknown>) {
  if (!s.value) return
  saving.value = true
  savedFlash.value = ''
  error.value = ''
  try {
    const body: Record<string, unknown> = { ...extra }
    const str = (f: string, j: string) => {
      if (drafts.value[f] !== undefined) body[j] = drafts.value[f]
    }
    str('provider', 'provider')
    str('model', 'model')
    if (drafts.value['max_tokens'] !== undefined) body['max_tokens'] = Number(drafts.value['max_tokens']) || 4096
    if (drafts.value['temperature'] !== undefined) body['temperature'] = Number(drafts.value['temperature']) || 0
    str('anthropic_base_url', 'anthropic_base_url')
    str('openai_base_url', 'openai_base_url')
    str('glm_base_url', 'glm_base_url')
    str('deepseek_base_url', 'deepseek_base_url')
    str('checkpoint_backend', 'checkpoint_backend')
    str('memory_backend', 'memory_backend')
    str('checkpoint_dir', 'checkpoint_dir')
    str('checkpoint_pg_host', 'checkpoint_pg_host')
    str('checkpoint_pg_port', 'checkpoint_pg_port')
    str('checkpoint_pg_db', 'checkpoint_pg_db')
    str('checkpoint_pg_user', 'checkpoint_pg_user')
    str('memory_pg_host', 'memory_pg_host')
    str('memory_pg_port', 'memory_pg_port')
    str('memory_pg_db', 'memory_pg_db')
    str('memory_pg_user', 'memory_pg_user')
    str('memory_engine', 'memory_engine')
    str('ladym_endpoint', 'ladym_endpoint')
    str('ladym_api_username', 'ladym_api_username')
    str('ladym_embedding', 'ladym_embedding')
    str('ladym_llm', 'ladym_llm')
    if (drafts.value['ladym_topk'] !== undefined) body['ladym_topk'] = Number(drafts.value['ladym_topk']) || 6
    if (drafts.value['debug_log'] !== undefined) body['debug_log'] = drafts.value['debug_log'] === 'true'
    if (drafts.value['ladym_autoretain'] !== undefined) body['ladym_autoretain'] = drafts.value['ladym_autoretain'] === 'true'
    if (drafts.value['ladym_system2'] !== undefined) body['ladym_system2'] = drafts.value['ladym_system2'] === 'true'
    if (drafts.value['ladym_system2_interval'] !== undefined)
      body['ladym_system2_interval'] = Number(drafts.value['ladym_system2_interval']) || 300
    // secrets: only non-empty drafts are forwarded (empty = keep stored)
    for (const [f, j] of [
      ['checkpoint_pg_password', 'checkpoint_pg_password'],
      ['memory_pg_password', 'memory_pg_password'],
      ['postgres_url', 'postgres_url'],
      ['ladym_api_password', 'ladym_api_password'],
      ['anthropic_api_key', 'anthropic_api_key'],
      ['openai_api_key', 'openai_api_key'],
      ['glm_api_key', 'glm_api_key'],
      ['deepseek_api_key', 'deepseek_api_key'],
    ] as const) {
      if (d(f)) body[j] = d(f)
    }
    // message plane
    str('msg_plane_mode', 'msg_plane_mode')
    str('msg_plane_nats_url', 'msg_plane_nats_url')
    str('msg_plane_subject', 'msg_plane_subject')

    const resp = await api('/api/settings', { method: 'PUT', body: JSON.stringify(body) })
    if (!resp.ok) {
      const doc = await resp.json().catch(() => ({}))
      throw new Error(doc.error || `${resp.status}`)
    }
    savedFlash.value = t('common.saved')
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    saving.value = false
  }
}

const sections = computed(() => [
  { id: 'model', label: t('nav.model') },
  { id: 'storage', label: t('nav.storage') },
  { id: 'memory', label: t('nav.memory') },
  { id: 'msgplane', label: t('nav.msgplane') },
  { id: 'ops', label: t('nav.ops') },
])
const active = ref('model')

function toggleLocale() {
  const next = locale.value === 'zh-CN' ? 'en' : 'zh-CN'
  locale.value = next
  localStorage.setItem('woca.configsvc.locale', next)
}

// expose to template sections via provide — reactive() unwraps the refs so
// the sections' template bindings read values, not Ref wrappers
provide('cfg', reactive({ s, d, v, setDraft, save, saving, savedFlash, error, api }) as unknown as CfgContext)

onMounted(() => {
  if (key.value) load()
})
</script>

<template>
  <div class="app">
    <header class="head">
      <div>
        <div class="title"><ServerCog :size="16" style="vertical-align: -3px" /> {{ t('title') }}</div>
        <div class="subtitle">{{ t('subtitle') }}</div>
      </div>
      <button class="ghost" @click="toggleLocale">{{ locale === 'zh-CN' ? 'EN' : '中文' }}</button>
    </header>

    <div v-if="!authed" class="login card">
      <div class="login-title">{{ t('login.title') }}</div>
      <input
        v-model="key"
        type="password"
        class="field wide"
        :placeholder="t('login.placeholder')"
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
        <ModelSection v-if="active === 'model'" />
        <StorageSection v-if="active === 'storage'" />
        <MemorySection v-if="active === 'memory'" />
        <MsgPlaneSection v-if="active === 'msgplane'" />
        <OpsSection v-if="active === 'ops'" />
        <div v-if="savedFlash" class="flash">
          <MessageSquare :size="12" style="vertical-align: -2px" /> {{ savedFlash }}
        </div>
      </main>
    </div>
  </div>
</template>

<style scoped>
.app { max-width: 880px; margin: 0 auto; padding: 24px 16px; font: 14px/1.5 system-ui, -apple-system, sans-serif; color: #1f2933; }
.head { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 20px; }
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
.flash { color: #047857; font-size: 12px; }
</style>
