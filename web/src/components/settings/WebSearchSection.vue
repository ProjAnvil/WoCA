<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { GripVertical, Search } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { getWebSearch, putWebSearch, testWebSearch } from '../../api/settings'
import type { WebSearchProvider, WebSearchState, WebSearchTestResult } from '../../types'

// The web-search chain: which backends are enabled, in what failover order.
// Reordering is drag-and-drop using native HTML5 events — the project has no
// drag dependency and adding one for a single list is not worth it.
const { t } = useI18n()

const state = ref<WebSearchState | null>(null)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const flash = ref('')

// Drafts. The order draft is what drag mutates; saving sends it wholesale,
// the same way the plugins list does.
const order = ref<string[]>([])
const searxngEndpoint = ref('')
const customEndpoint = ref('')
const customKey = ref('')
const allowedDomains = ref('')
const deniedDomains = ref('')

const testing = ref(false)
const testResult = ref<WebSearchTestResult | null>(null)
const testQuery = ref('')

const dragIndex = ref<number | null>(null)

const locked = computed(() => new Set(state.value?.managed_fields ?? []))
const providersLocked = computed(() => locked.value.has('search-providers'))

const ordered = computed<WebSearchProvider[]>(() => {
  const all = state.value?.providers ?? []
  const rank = new Map(order.value.map((id, i) => [id, i]))
  const enabledRows = order.value
    .map((id) => all.find((p) => p.id === id))
    .filter((p): p is WebSearchProvider => Boolean(p))
  const rest = all.filter((p) => !rank.has(p.id))
  return [...enabledRows, ...rest]
})

const enabledSet = computed(() => new Set(order.value))

function enabledOf(id: string): boolean {
  return enabledSet.value.has(id)
}

function toggle(id: string) {
  if (providersLocked.value) return
  if (enabledOf(id)) {
    order.value = order.value.filter((x) => x !== id)
  } else {
    order.value = [...order.value, id]
  }
  flash.value = ''
}

// --- drag reordering ---

function onDragStart(index: number) {
  if (providersLocked.value) return
  dragIndex.value = index
}

function onDragEnter(index: number) {
  if (dragIndex.value === null || dragIndex.value === index) return
  const from = dragIndex.value
  const next = [...order.value]
  // Only the enabled rows are draggable, so indices stay inside the list.
  const moved = next.splice(from, 1)[0]
  next.splice(index, 0, moved)
  order.value = next
  dragIndex.value = index
  flash.value = ''
}

function onDragEnd() {
  dragIndex.value = null
}

// Index within the enabled list (drag operates on that list only).
function enabledIndex(id: string): number {
  return order.value.indexOf(id)
}

// --- load / save ---

async function load() {
  loading.value = true
  error.value = ''
  try {
    const s = await getWebSearch()
    state.value = s
    order.value = [...s.enabled]
    searxngEndpoint.value = s.searxng_endpoint
    customEndpoint.value = s.custom_endpoint
    allowedDomains.value = s.allowed_domains.join(', ')
    deniedDomains.value = s.denied_domains.join(', ')
    customKey.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

function parseList(v: string): string[] {
  return v
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

async function save() {
  saving.value = true
  error.value = ''
  flash.value = ''
  try {
    await putWebSearch({
      providers: order.value,
      searxng_endpoint: searxngEndpoint.value.trim(),
      custom_endpoint: customEndpoint.value.trim(),
      custom_key: customKey.value || undefined,
      allowed_domains: parseList(allowedDomains.value),
      denied_domains: parseList(deniedDomains.value),
    })
    customKey.value = ''
    flash.value = t('settings.websearch.saved')
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    saving.value = false
  }
}

async function runTest() {
  testing.value = true
  error.value = ''
  testResult.value = null
  try {
    testResult.value = await testWebSearch(testQuery.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    testing.value = false
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="setting-section">
    <div class="setting-section-title">
      <Search :size="13" style="vertical-align: -2px" />
      {{ t('settings.websearch.title') }}
    </div>
    <p class="mem-intro">{{ t('settings.websearch.intro') }}</p>

    <div v-if="loading && !state" class="row-hint">{{ t('common.loading') }}</div>
    <div v-else-if="error" class="row-hint" style="color: var(--danger)">{{ error }}</div>

    <template v-if="state">
      <!-- Backend list: drag to set the failover order, switch to enable -->
      <div class="setting-block">
        <div class="setting-block-title">{{ t('settings.websearch.backends') }}</div>
        <p class="row-hint">{{ t('settings.websearch.backendsHint') }}</p>
        <div v-if="providersLocked" class="row-hint" style="margin-bottom: 6px">
          {{ t('settings.managedTooltip') }}
        </div>

        <div
          v-for="p in ordered"
          :key="p.id"
          class="ws-row"
          :class="{ 'ws-row-on': enabledOf(p.id), 'ws-row-locked': providersLocked }"
          :data-testid="'ws-row-' + p.id"
        >
          <span
            v-if="enabledOf(p.id)"
            class="ws-grip"
            :aria-hidden="!enabledOf(p.id)"
            :draggable="!providersLocked"
            @dragstart="onDragStart(enabledIndex(p.id))"
            @dragenter.prevent="onDragEnter(enabledIndex(p.id))"
            @dragover.prevent
            @dragend="onDragEnd"
          >
            <GripVertical :size="13" />
          </span>
          <span v-else class="ws-grip ws-grip-off"><GripVertical :size="13" /></span>

          <div class="ws-copy">
            <div class="ws-name">
              {{ p.name }}
              <span v-if="enabledOf(p.id)" class="ws-order">#{{ enabledIndex(p.id) + 1 }}</span>
              <span v-if="p.needs_endpoint" class="plugin-badge">
                {{ t('settings.websearch.needsEndpoint') }}
              </span>
            </div>
            <div class="row-hint">{{ p.description }}</div>
          </div>

          <button
            class="switch"
            role="switch"
            :class="{ on: enabledOf(p.id) }"
            :disabled="providersLocked || saving"
            :aria-label="p.name"
            :aria-checked="enabledOf(p.id)"
            :data-testid="'ws-switch-' + p.id"
            @click="toggle(p.id)"
          ></button>
        </div>
      </div>

      <!-- Endpoints for the backends that need one -->
      <div v-if="enabledOf('searxng') || enabledOf('custom')" class="setting-block">
        <div class="setting-block-title">{{ t('settings.websearch.endpoints') }}</div>

        <div v-if="enabledOf('searxng')" class="ws-field">
          <label class="ws-label" for="ws-searxng">
            {{ t('settings.websearch.searxngEndpoint') }}
          </label>
          <input
            id="ws-searxng"
            v-model="searxngEndpoint"
            class="field"
            :disabled="locked.has('search-searxng-endpoint') || saving"
            :placeholder="t('settings.websearch.searxngPlaceholder')"
          />
          <div class="row-hint">{{ t('settings.websearch.searxngHint') }}</div>
        </div>

        <div v-if="enabledOf('custom')" class="ws-field">
          <label class="ws-label" for="ws-custom">
            {{ t('settings.websearch.customEndpoint') }}
          </label>
          <input
            id="ws-custom"
            v-model="customEndpoint"
            class="field"
            :disabled="locked.has('search-custom-endpoint') || saving"
            placeholder="https://search.internal/api?q={q}"
          />

          <label class="ws-label" for="ws-custom-key">
            {{ t('settings.websearch.customKey') }}
          </label>
          <input
            id="ws-custom-key"
            v-model="customKey"
            class="field"
            type="password"
            autocomplete="off"
            :disabled="locked.has('search-custom-key') || saving"
            :placeholder="
              state.custom_key_set
                ? t('settings.websearch.customKeyStored')
                : t('settings.websearch.customKeyEmpty')
            "
          />
        </div>
      </div>

      <!-- Domain filters -->
      <div class="setting-block">
        <div class="setting-block-title">{{ t('settings.websearch.domains') }}</div>
        <div class="ws-field">
          <label class="ws-label" for="ws-allow">
            {{ t('settings.websearch.allowedDomains') }}
          </label>
          <input
            id="ws-allow"
            v-model="allowedDomains"
            class="field"
            :disabled="locked.has('search-allowed-domains') || saving"
            placeholder="go.dev, pkg.go.dev"
          />
        </div>
        <div class="ws-field">
          <label class="ws-label" for="ws-deny">
            {{ t('settings.websearch.deniedDomains') }}
          </label>
          <input
            id="ws-deny"
            v-model="deniedDomains"
            class="field"
            :disabled="locked.has('search-denied-domains') || saving"
            placeholder="spam.example"
          />
          <div class="row-hint">{{ t('settings.websearch.domainsHint') }}</div>
        </div>
      </div>

      <!-- Verify: the only way to tell "my settings are wrong" from "the
           internet is broken", because a blocked backend looks exactly like
           a search that found nothing. -->
      <div class="setting-block">
        <div class="setting-block-title">{{ t('settings.websearch.test') }}</div>
        <div class="ws-field">
          <input
            v-model="testQuery"
            class="field"
            :placeholder="t('settings.websearch.testPlaceholder')"
          />
        </div>
        <div class="ws-actions">
          <button class="btn" data-testid="ws-test" :disabled="testing || saving" @click="runTest">
            {{ testing ? t('settings.websearch.testing') : t('settings.websearch.testAction') }}
          </button>
          <button class="btn btn-primary" data-testid="ws-save" :disabled="saving || testing" @click="save">
            {{ saving ? t('settings.websearch.saving') : t('settings.footer.save') }}
          </button>
        </div>

        <div v-if="testResult" class="ws-test" :class="{ 'ws-test-bad': !testResult.ok }">
          <div v-if="testResult.ok">
            {{
              t('settings.websearch.testOk', {
                provider: testResult.provider || '?',
                results: testResult.results,
                ms: testResult.latency_ms,
              })
            }}
            <div v-if="testResult.sample" class="row-hint">{{ testResult.sample }}</div>
          </div>
          <div v-else>
            <div>{{ t('settings.websearch.testFailed') }}</div>
            <div class="row-hint">{{ testResult.error }}</div>
            <div class="row-hint">{{ t('settings.websearch.testFailedHint') }}</div>
          </div>
        </div>
        <div v-if="flash" class="row-hint" style="margin-top: 6px">{{ flash }}</div>
      </div>
    </template>
  </section>
</template>

<style scoped>
.ws-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 4px;
  border-bottom: 1px solid var(--border-subtle, rgba(128, 128, 128, 0.16));
}
.ws-row-on {
  background: var(--accent-soft, rgba(120, 170, 255, 0.08));
}
.ws-row-locked {
  opacity: 0.62;
}
.ws-grip {
  cursor: grab;
  color: var(--fg-dim, #8a8a8a);
  display: inline-flex;
}
.ws-grip-off {
  opacity: 0.25;
  cursor: default;
}
.ws-copy {
  flex: 1;
  min-width: 0;
}
.ws-name {
  font-size: 12px;
  display: flex;
  align-items: center;
  gap: 6px;
}
.ws-order {
  font-size: 10px;
  color: var(--fg-dim, #8a8a8a);
}
.ws-field {
  margin-bottom: 8px;
}
.ws-label {
  display: block;
  font-size: 11px;
  margin-bottom: 3px;
  color: var(--fg-dim, #8a8a8a);
}
.ws-actions {
  display: flex;
  gap: 8px;
  align-items: center;
}
.ws-test {
  margin-top: 8px;
  padding: 8px;
  border: 1px solid var(--border-subtle, rgba(128, 128, 128, 0.2));
  border-radius: 6px;
  font-size: 12px;
}
.ws-test-bad {
  border-color: var(--danger);
}
</style>
