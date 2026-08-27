<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Search } from 'lucide-vue-next'
import { usePaletteStore } from '../../stores/palette'
import { useFocusTrap } from '../../composables/useFocusTrap'
import { fuzzyScore } from '../../lib/fuzzy'
import type { PaletteCommand } from '../../composables/useAppCommands'

const props = defineProps<{ commands: PaletteCommand[] }>()
const { t } = useI18n()
const palette = usePaletteStore()

const root = ref<HTMLElement | null>(null)
const active = computed(() => palette.open)
useFocusTrap(root, active, { esc: () => palette.close() })

const query = ref('')
const index = ref(0)
const inputEl = ref<HTMLInputElement | null>(null)

watch(
  () => palette.open,
  async (open) => {
    if (open) {
      query.value = ''
      index.value = 0
      await nextTick()
      inputEl.value?.focus()
    }
  },
)

const filtered = computed(() => {
  const q = query.value.trim()
  if (!q) return props.commands
  return props.commands
    .map((c) => ({ c, s: fuzzyScore(q, `${c.label} ${c.keywords ?? ''} ${c.hint ?? ''}`) }))
    .filter((x) => x.s !== null)
    .sort((a, b) => (b.s as number) - (a.s as number))
    .map((x) => x.c)
})

// Display groups keep their natural order; navigation runs over the flat
// filtered list so ↑↓ cross group boundaries.
const grouped = computed(() => {
  const out: { group: PaletteCommand['group']; items: PaletteCommand[] }[] = []
  for (const c of filtered.value) {
    const last = out[out.length - 1]
    if (last && last.group === c.group) last.items.push(c)
    else out.push({ group: c.group, items: [c] })
  }
  return out
})

watch(filtered, () => {
  index.value = 0
})

function move(dir: 1 | -1) {
  const n = filtered.value.length
  if (!n) return
  index.value = (index.value + dir + n) % n
  document.getElementById(`w-cmd-opt-${index.value}`)?.scrollIntoView({ block: 'nearest' })
}

function execute(cmd: PaletteCommand) {
  palette.close()
  cmd.run()
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    move(1)
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    move(-1)
  } else if (e.key === 'Enter') {
    e.preventDefault()
    const cmd = filtered.value[index.value]
    if (cmd) execute(cmd)
  }
}
</script>

<template>
  <Teleport to="body">
    <Transition name="w-palette">
      <div v-if="palette.open" class="w-palette-backdrop" @click.self="palette.close()">
        <div
          ref="root"
          class="w-palette"
          role="dialog"
          aria-modal="true"
          :aria-label="t('palette.title')"
          tabindex="-1"
        >
          <div class="w-palette-input-row">
            <Search :size="15" class="w-palette-search-icon" aria-hidden="true" />
            <input
              ref="inputEl"
              v-model="query"
              class="w-palette-input"
              role="combobox"
              :aria-expanded="filtered.length ? 'true' : 'false'"
              aria-controls="w-palette-list"
              :aria-activedescendant="filtered.length ? `w-cmd-opt-${index}` : undefined"
              :placeholder="t('palette.placeholder')"
              type="text"
              autocomplete="off"
              spellcheck="false"
              @keydown="onKeydown"
            />
          </div>
          <div id="w-palette-list" class="w-palette-list" role="listbox" :aria-label="t('palette.title')">
            <template v-for="g in grouped" :key="g.group + g.items[0]?.id">
              <div class="w-palette-group">{{ t(`palette.groups.${g.group}`) }}</div>
              <button
                v-for="c in g.items"
                :id="`w-cmd-opt-${filtered.indexOf(c)}`"
                :key="c.id"
                class="w-palette-item"
                :class="{ active: filtered.indexOf(c) === index }"
                role="option"
                :aria-selected="filtered.indexOf(c) === index"
                type="button"
                @click="execute(c)"
                @mousemove="index = filtered.indexOf(c)"
              >
                <span class="w-palette-item-label">{{ c.label }}</span>
                <span v-if="c.hint" class="w-palette-item-hint">{{ c.hint }}</span>
              </button>
            </template>
            <div v-if="!filtered.length" class="w-palette-empty">{{ t('palette.noResults') }}</div>
          </div>
          <div class="w-palette-foot" aria-hidden="true">
            <span><kbd>↑</kbd><kbd>↓</kbd> {{ t('palette.navigate') }}</span>
            <span><kbd>↵</kbd> {{ t('palette.run') }}</span>
            <span><kbd>esc</kbd> {{ t('palette.close') }}</span>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.w-palette-backdrop {
  position: fixed;
  inset: 0;
  z-index: var(--z-palette);
  background: var(--scrim);
  display: flex;
  justify-content: center;
  align-items: flex-start;
  padding-top: 20vh;
}
.w-palette {
  width: min(640px, 92vw);
  max-height: 60vh;
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-overlay);
  overflow: hidden;
  outline: none;
}
.w-palette-input-row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border);
}
.w-palette-search-icon {
  color: var(--muted);
  flex-shrink: 0;
}
.w-palette-input {
  flex: 1;
  border: none;
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: var(--text-md);
  outline: none;
}
.w-palette-input::placeholder {
  color: var(--faint);
}
.w-palette-list {
  flex: 1;
  overflow-y: auto;
  padding: var(--space-2);
}
.w-palette-group {
  padding: var(--space-3) var(--space-4) var(--space-1);
  font-size: var(--text-xs);
  color: var(--muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.w-palette-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
  width: 100%;
  padding: var(--space-3) var(--space-4);
  border: none;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  font: inherit;
  font-size: var(--text-base);
  text-align: left;
  cursor: pointer;
}
.w-palette-item.active,
.w-palette-item:not(:disabled):hover {
  background: var(--surface-hover);
  color: var(--text);
}
.w-palette-item:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: -2px;
}
.w-palette-item-label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.w-palette-item-hint {
  flex-shrink: 0;
  font-size: var(--text-xs);
  color: var(--faint);
}
.w-palette-empty {
  padding: var(--space-6);
  text-align: center;
  color: var(--muted);
  font-size: var(--text-sm);
}
.w-palette-foot {
  display: flex;
  gap: var(--space-5);
  padding: var(--space-3) var(--space-5);
  border-top: 1px solid var(--border);
  font-size: var(--text-xs);
  color: var(--muted);
}
.w-palette-foot kbd {
  display: inline-block;
  min-width: 18px;
  padding: 1px 4px;
  margin-right: 2px;
  border: 1px solid var(--border);
  border-bottom-width: 2px;
  border-radius: var(--radius-xs);
  background: var(--surface-2);
  font-family: var(--font-mono);
  font-size: 10px;
  text-align: center;
}

.w-palette-enter-active,
.w-palette-leave-active {
  transition: opacity var(--dur-base) var(--ease-out);
}
.w-palette-enter-active .w-palette {
  transition: transform var(--dur-base) var(--ease-out);
}
.w-palette-enter-from,
.w-palette-leave-to {
  opacity: 0;
}
.w-palette-enter-from .w-palette {
  transform: translateY(-8px) scale(0.99);
}
</style>
