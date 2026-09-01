<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'

// Operator-delivered skills & MCP (product ruling): the admin authors
// them here; enrolled bridges pull them with the config package and
// materialize them as their LOWEST merge layer — users can still
// configure their own on their bridge, and their config wins.
const { t } = useI18n()
const { api } = injectCfg()

interface Skill {
  name: string
  description: string
  body: string
  updated_at: number
}
interface McpServer {
  name: string
  spec: string
  updated_at: number
}

const skills = ref<Skill[]>([])
const servers = ref<McpServer[]>([])
const loading = ref(true)
const error = ref('')

const newSkill = ref({ name: '', description: '', body: '' })
const newServer = ref({ name: '', spec: '' })
const savingSkill = ref(false)
const savingServer = ref(false)
const deleting = ref('')

async function load() {
  error.value = ''
  try {
    const [s, m] = await Promise.all([api('/api/skills'), api('/api/mcp')])
    if (s.ok) skills.value = (await s.json()) ?? []
    if (m.ok) servers.value = (await m.json()) ?? []
  } catch (e) {
    error.value = String(e)
  } finally {
    loading.value = false
  }
}

async function saveSkill() {
  const sk = newSkill.value
  if (!sk.name.trim() || !sk.body.trim() || savingSkill.value) return
  savingSkill.value = true
  error.value = ''
  try {
    const resp = await api('/api/skills', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sk),
    })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    newSkill.value = { name: '', description: '', body: '' }
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    savingSkill.value = false
  }
}

async function saveServer() {
  const m = newServer.value
  if (!m.name.trim() || !m.spec.trim() || savingServer.value) return
  savingServer.value = true
  error.value = ''
  try {
    const resp = await api('/api/mcp', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(m),
    })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    newServer.value = { name: '', spec: '' }
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    savingServer.value = false
  }
}

async function remove(kind: 'skills' | 'mcp', name: string) {
  if (deleting.value) return
  if (!confirm(t('skills.deleteConfirm', { name }))) return
  deleting.value = name
  error.value = ''
  try {
    const resp = await api(`/api/${kind}/${encodeURIComponent(name)}`, { method: 'DELETE' })
    if (!resp.ok) throw new Error((await resp.json().catch(() => ({ error: resp.status }))).error ?? `${resp.status}`)
    await load()
  } catch (e) {
    error.value = String(e)
  } finally {
    deleting.value = ''
  }
}

function fmtTime(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}

onMounted(load)
</script>

<template>
  <section class="sk-card">
    <div class="sk-title">{{ t('skills.title') }}</div>
    <p class="sk-hint">{{ t('skills.hint') }}</p>
    <p v-if="error" class="sk-error">{{ error }}</p>

    <div class="sk-block">
      <div class="sk-subtitle">{{ t('skills.skillsTitle') }}</div>
      <div class="sk-form">
        <input v-model="newSkill.name" :placeholder="t('skills.namePlaceholder')" :disabled="savingSkill" />
        <input v-model="newSkill.description" :placeholder="t('skills.descPlaceholder')" :disabled="savingSkill" />
        <textarea
          v-model="newSkill.body"
          rows="5"
          :placeholder="t('skills.bodyPlaceholder')"
          :disabled="savingSkill"
          @keyup.ctrl-enter="saveSkill"
        ></textarea>
        <button class="btn btn-primary" :disabled="savingSkill || !newSkill.name.trim() || !newSkill.body.trim()" @click="saveSkill">
          {{ savingSkill ? t('skills.saving') : t('skills.save') }}
        </button>
      </div>
      <table v-if="skills.length" class="sk-table">
        <thead>
          <tr>
            <th>{{ t('skills.colName') }}</th>
            <th>{{ t('skills.colDesc') }}</th>
            <th>{{ t('skills.colUpdated') }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="sk in skills" :key="sk.name">
            <td><code>{{ sk.name }}</code></td>
            <td class="sk-desc">{{ sk.description }}</td>
            <td>{{ fmtTime(sk.updated_at) }}</td>
            <td>
              <button class="btn btn-danger" :disabled="deleting === sk.name" @click="remove('skills', sk.name)">
                {{ t('skills.delete') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else-if="!loading" class="sk-hint">{{ t('skills.empty') }}</p>
    </div>

    <div class="sk-block">
      <div class="sk-subtitle">{{ t('skills.mcpTitle') }}</div>
      <p class="sk-hint">{{ t('skills.mcpHint') }}</p>
      <div class="sk-form">
        <input v-model="newServer.name" :placeholder="t('skills.mcpNamePlaceholder')" :disabled="savingServer" />
        <input v-model="newServer.spec" :placeholder="t('skills.mcpSpecPlaceholder')" :disabled="savingServer" @keyup.enter="saveServer" />
        <button class="btn btn-primary" :disabled="savingServer || !newServer.name.trim() || !newServer.spec.trim()" @click="saveServer">
          {{ savingServer ? t('skills.saving') : t('skills.save') }}
        </button>
      </div>
      <table v-if="servers.length" class="sk-table">
        <thead>
          <tr>
            <th>{{ t('skills.colName') }}</th>
            <th>spec</th>
            <th>{{ t('skills.colUpdated') }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in servers" :key="m.name">
            <td><code>{{ m.name }}</code></td>
            <td><code class="sk-spec">{{ m.spec }}</code></td>
            <td>{{ fmtTime(m.updated_at) }}</td>
            <td>
              <button class="btn btn-danger" :disabled="deleting === m.name" @click="remove('mcp', m.name)">
                {{ t('skills.delete') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else-if="!loading" class="sk-hint">{{ t('skills.mcpEmpty') }}</p>
    </div>
  </section>
</template>

<style scoped>
.sk-card { border: 1px solid #e4e7eb; border-radius: 10px; padding: 12px 14px; }
.sk-title { font-weight: 600; margin-bottom: 6px; }
.sk-subtitle { font-weight: 600; margin: 12px 0 6px; }
.sk-hint { margin: 0 0 8px; font-size: 12.5px; opacity: 0.75; }
.sk-error { color: #e5534b; font-size: 12.5px; margin: 0 0 8px; }
.sk-block { margin-bottom: 8px; }
.sk-form { display: flex; flex-direction: column; gap: 6px; margin-bottom: 10px; }
.sk-form input, .sk-form textarea {
  padding: 7px 10px; border: 1px solid #cbd2d9; border-radius: 8px;
  background: transparent; color: inherit; font: inherit; font-size: 13px;
}
.sk-form textarea { font-family: ui-monospace, monospace; font-size: 12px; resize: vertical; }
.sk-form .btn-primary { align-self: flex-start; }
.sk-table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
.sk-table th { text-align: left; opacity: 0.6; font-weight: 500; padding: 4px 8px 4px 0; }
.sk-table td { padding: 5px 8px 5px 0; border-top: 1px solid #e4e7eb; }
.sk-desc { max-width: 280px; }
.sk-spec { font-family: ui-monospace, monospace; font-size: 11.5px; overflow-wrap: anywhere; max-width: 300px; }
.btn { border: 1px solid #cbd2d9; border-radius: 6px; background: none; padding: 5px 10px; cursor: pointer; font: inherit; font-size: 12.5px; }
.btn-primary { border-color: #111; color: #111; }
.btn-danger { border-color: #c0392b; color: #c0392b; }
</style>
