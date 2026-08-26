<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Globe, RefreshCw, Trash2 } from 'lucide-vue-next'
import {
  addWebProfile,
  deleteWebProfile,
  getWebProfiles,
  useWebProfile,
  type WebProfile,
} from '../../api/client'

// Remote profiles (bridge 形态): the saved remote origins this bridge's
// embedded web can be pointed at — stored in the bridge's own settings
// database, so the list follows the bridge rather than the browser.
// Switching is a full navigation: the target origin serves its own web
// and its own login gate; the session cookie stays with each origin.
const { t } = useI18n()

const profiles = ref<WebProfile[]>([])
const last = ref('')
const addURL = ref('')
const addLabel = ref('')
const addError = ref('')
const busy = ref(false)

async function refresh() {
  busy.value = true
  try {
    const data = await getWebProfiles()
    profiles.value = data.profiles
    last.value = data.last ?? ''
    addError.value = ''
  } catch (e) {
    addError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

onMounted(refresh)

// Mark the profile last-used, then navigate: the reload re-fetches the
// SPA from the target origin (its login gate appears when unauthenticated).
async function pick(p: WebProfile) {
  if (busy.value) return
  busy.value = true
  try {
    await useWebProfile(p.id)
  } catch {
    // Persisting the pointer is best-effort; the navigation still goes.
  }
  window.location.href = p.url
}

async function add() {
  const v = addURL.value.trim()
  addError.value = ''
  if (!v || busy.value) return
  busy.value = true
  try {
    await addWebProfile(v, addLabel.value.trim())
    addURL.value = ''
    addLabel.value = ''
    await refresh()
  } catch (e) {
    addError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

async function remove(p: WebProfile) {
  if (busy.value) return
  busy.value = true
  try {
    await deleteWebProfile(p.id)
    await refresh()
  } catch (e) {
    addError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="setting-section">
    <div class="setting-section-title">
      <Globe :size="13" style="vertical-align: -2px" /> {{ t('settings.remotes.title') }}
      <button class="icon-btn edge-refresh" :title="t('settings.remotes.refresh')" @click="refresh()">
        <RefreshCw :size="13" />
      </button>
    </div>
    <p class="mem-intro">{{ t('settings.remotes.intro') }}</p>

    <div class="edge-list">
      <div v-for="p in profiles" :key="p.id" class="edge-bridge" :class="{ current: p.id === last }">
        <button class="edge-id" style="background: none; border: 0; cursor: pointer; text-align: left" :data-testid="`remote-profile-${p.id}`" @click="pick(p)">
          <span class="edge-id">{{ p.label || p.url }}</span>
          <span v-if="p.id === last" class="edge-current">{{ t('settings.remotes.lastUsed') }}</span>
        </button>
        <button class="icon-btn" :title="t('settings.remotes.remove')" :data-testid="`remote-remove-${p.id}`" @click="remove(p)">
          <Trash2 :size="13" />
        </button>
      </div>
      <p v-if="!profiles.length && !addError" class="row-hint">{{ t('settings.remotes.empty') }}</p>
    </div>

    <div class="setting-row edge-add">
      <div>
        <div class="row-label">{{ t('settings.remotes.addLabel') }}</div>
        <div class="row-hint">{{ t('settings.remotes.addHint') }}</div>
        <div v-if="addError" class="row-hint" style="color: var(--danger, #e5484d)">{{ addError }}</div>
        <input v-model="addURL" class="field" style="max-width: 280px" :placeholder="t('settings.remotes.addPlaceholder')" @keyup.enter="add()" />
        <input v-model="addLabel" class="field" style="max-width: 160px" :placeholder="t('settings.remotes.labelPlaceholder')" @keyup.enter="add()" />
      </div>
      <button class="btn" :disabled="!addURL.trim() || busy" @click="add()">{{ t('settings.remotes.addButton') }}</button>
    </div>
  </section>
</template>
