<script setup lang="ts">
import { computed, ref } from 'vue'
import { GripVertical, Search } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'

// The enterprise web-search chain. It rides the shared settings surface, so
// drafts go through the console's own save plumbing — the same contract every
// other section here uses.
//
// One difference from the personal form: an empty value here means "not
// managed" (users choose for themselves), not "off". Filling a field in pins
// it on every machine the pack reaches.
const { t } = useI18n()
const cfg = injectCfg()

interface Backend {
  id: string
  name: string
  description: string
  needsEndpoint: boolean
}

const catalog = (): Backend[] => [
  {
    id: 'ddg-html',
    name: 'DuckDuckGo',
    description: 'No account needed. Blocked from many managed networks.',
    needsEndpoint: false,
  },
  {
    id: 'ddg-lite',
    name: 'DuckDuckGo (lightweight)',
    description: 'A different endpoint, useful as a fallback.',
    needsEndpoint: false,
  },
  {
    id: 'searxng',
    name: 'SearXNG',
    description: 'Self-hosted or public instance.',
    needsEndpoint: true,
  },
  {
    id: 'tavily',
    name: 'Tavily (keyless)',
    description: 'No key needed, but queries go to a third party.',
    needsEndpoint: false,
  },
  {
    id: 'custom',
    name: 'Custom endpoint',
    description: 'Any JSON search API you host.',
    needsEndpoint: true,
  },
]

// Draft order. Empty until the operator picks something, which is what keeps
// the setting "not managed".
const order = ref<string[]>([...(cfg.s?.search_providers ?? [])])
const dragIndex = ref<number | null>(null)

const ordered = computed(() => {
  const rank = new Map(order.value.map((id, i) => [id, i]))
  const chosen = order.value
    .map((id) => catalog().find((b) => b.id === id))
    .filter((b): b is Backend => Boolean(b))
  return [...chosen, ...catalog().filter((b) => !rank.has(b.id))]
})

const enabled = (id: string) => order.value.includes(id)
const posOf = (id: string) => order.value.indexOf(id)

function toggle(id: string) {
  order.value = enabled(id) ? order.value.filter((x) => x !== id) : [...order.value, id]
}

function onDragStart(i: number) {
  dragIndex.value = i
}

function onDragEnter(i: number) {
  if (dragIndex.value === null || dragIndex.value === i) return
  const next = [...order.value]
  next.splice(i, 0, next.splice(dragIndex.value, 1)[0])
  order.value = next
  dragIndex.value = i
}

function onDragEnd() {
  dragIndex.value = null
}

// --- probe ---

const testing = ref(false)
const probe = ref<{
  ok: boolean
  provider?: string
  results: number
  latency_ms: number
  sample?: string
  error?: string
} | null>(null)
const probeError = ref('')

async function runTest() {
  testing.value = true
  probe.value = null
  probeError.value = ''
  try {
    const res = await cfg.api('/api/websearch/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ query: '' }),
    })
    probe.value = await res.json()
  } catch (e) {
    probeError.value = e instanceof Error ? e.message : String(e)
  } finally {
    testing.value = false
  }
}

async function save() {
  await cfg.save({
    search_providers: order.value,
    search_allowed_domains: parseList(cfg.v('search_allowed_domains')),
    search_denied_domains: parseList(cfg.v('search_denied_domains')),
  })
}

function parseList(v: string): string[] {
  return (v ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}
</script>

<template>
  <section class="card">
    <div class="card-head">
      <Search :size="13" style="vertical-align: -2px" /> {{ t('nav.websearch') }}
    </div>
    <p class="hint">{{ t('websearch.intro') }}</p>

    <div class="block">
      <div class="block-title">{{ t('websearch.backends') }}</div>
      <p class="hint">{{ t('websearch.backendsHint') }}</p>

      <div
        v-for="b in ordered"
        :key="b.id"
        class="ws-row"
        :class="{ 'ws-row-on': enabled(b.id) }"
      >
        <span
          v-if="enabled(b.id)"
          class="ws-grip"
          draggable="true"
          @dragstart="onDragStart(posOf(b.id))"
          @dragenter.prevent="onDragEnter(posOf(b.id))"
          @dragover.prevent
          @dragend="onDragEnd"
        >
          <GripVertical :size="13" />
        </span>
        <span v-else class="ws-grip ws-grip-off"><GripVertical :size="13" /></span>

        <div class="ws-copy">
          <div class="ws-name">
            {{ b.name }}
            <span v-if="enabled(b.id)" class="ws-order">#{{ posOf(b.id) + 1 }}</span>
            <span v-if="b.needsEndpoint" class="badge">{{ t('websearch.needsEndpoint') }}</span>
          </div>
          <div class="hint">{{ b.description }}</div>
        </div>

        <button
          class="switch"
          role="switch"
          :class="{ on: enabled(b.id) }"
          :disabled="cfg.saving"
          :aria-label="b.name"
          :aria-checked="enabled(b.id)"
          @click="toggle(b.id)"
        ></button>
      </div>
    </div>

    <div class="block">
      <div class="block-title">{{ t('websearch.searxng') }}</div>
      <input
        class="field"
        :value="cfg.v('search_searxng_endpoint')"
        :placeholder="t('websearch.searxngPlaceholder')"
        :disabled="cfg.saving"
        @input="cfg.setDraft('search_searxng_endpoint', ($event.target as HTMLInputElement).value)"
      />
    </div>

    <div class="block">
      <div class="block-title">{{ t('websearch.custom') }}</div>
      <input
        class="field"
        :value="cfg.v('search_custom_endpoint')"
        placeholder="https://search.internal/api?q={q}"
        :disabled="cfg.saving"
        @input="cfg.setDraft('search_custom_endpoint', ($event.target as HTMLInputElement).value)"
      />
      <input
        class="field"
        type="password"
        autocomplete="off"
        :placeholder="cfg.s?.search_custom_key_set ? t('websearch.customKeySet') : t('websearch.customKey')"
        :disabled="cfg.saving"
        @input="cfg.setDraft('search_custom_key', ($event.target as HTMLInputElement).value)"
      />
    </div>

    <div class="block">
      <div class="block-title">{{ t('websearch.allowed') }}</div>
      <input
        class="field"
        :value="cfg.v('search_allowed_domains')"
        placeholder="go.dev, pkg.go.dev"
        :disabled="cfg.saving"
        @input="cfg.setDraft('search_allowed_domains', ($event.target as HTMLInputElement).value)"
      />
      <div class="block-title">{{ t('websearch.denied') }}</div>
      <input
        class="field"
        :value="cfg.v('search_denied_domains')"
        placeholder="spam.example"
        :disabled="cfg.saving"
        @input="cfg.setDraft('search_denied_domains', ($event.target as HTMLInputElement).value)"
      />
      <p class="hint">{{ t('websearch.domainsHint') }}</p>
    </div>

    <!-- Verify before shipping: a chain that cannot answer looks identical to
         one that simply found nothing. -->
    <div class="block">
      <div class="block-title">{{ t('websearch.test') }}</div>
      <div class="row">
        <button class="btn" :disabled="testing || cfg.saving" @click="runTest">
          {{ testing ? t('websearch.testing') : t('websearch.testAction') }}
        </button>
        <button class="btn btn-primary" :disabled="cfg.saving || testing" @click="save">
          {{ cfg.saving ? t('websearch.saving') : t('websearch.save') }}
        </button>
      </div>

      <div v-if="probe" class="ws-test" :class="{ 'ws-test-bad': !probe.ok }">
        <div v-if="probe.ok">
          {{
            t('websearch.testOk', {
              provider: probe.provider || '?',
              results: probe.results,
              ms: probe.latency_ms,
            })
          }}
          <div v-if="probe.sample" class="hint">{{ probe.sample }}</div>
        </div>
        <div v-else>
          <div>{{ t('websearch.testFailed') }}</div>
          <div class="hint">{{ probe.error }}</div>
        </div>
      </div>
      <div v-if="probeError" class="hint" style="color: var(--danger)">{{ probeError }}</div>
    </div>
  </section>
</template>

<style scoped>
.ws-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 4px;
  border-bottom: 1px solid rgba(128, 128, 128, 0.16);
}
.ws-row-on {
  background: rgba(120, 170, 255, 0.08);
}
.ws-grip {
  cursor: grab;
  display: inline-flex;
  opacity: 0.7;
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
  opacity: 0.7;
}
.ws-test {
  margin-top: 8px;
  padding: 8px;
  border: 1px solid rgba(128, 128, 128, 0.2);
  border-radius: 6px;
  font-size: 12px;
}
.ws-test-bad {
  border-color: var(--danger);
}
</style>
