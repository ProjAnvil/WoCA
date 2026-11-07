<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Copy } from 'lucide-vue-next'
import { useActivityStore, type OutputKind } from '../../stores/activity'
import { useToastStore } from '../../stores/toast'
import { fmtBytes } from '../../lib/format'

// One item's output tail (PM §4.9): mono text, truncation note, copy. The
// watcher lifecycle mirrors the expansion state — mounted on expand, torn
// down on collapse; the store itself stops polling once the item ends or
// the page hides (AC23).
const props = defineProps<{ sid: string; kind: OutputKind; id: string }>()

const { t } = useI18n()
const activity = useActivityStore()
const toast = useToastStore()

let stop: { stop(): void } | null = null

const tail = computed(() => activity.outputOf(props.sid, props.kind, props.id))
const error = computed(() => activity.outputErrorOf(props.sid, props.kind, props.id))

// Also the retry path after a failed read: watchOutput supersedes any
// previous watcher for the same key, so restarting here never orphans one.
function start(): void {
  stop = activity.watchOutput(props.sid, props.kind, props.id)
}

onMounted(start)
onBeforeUnmount(() => stop?.stop())

async function copy(): Promise<void> {
  try {
    await navigator.clipboard.writeText(tail.value?.output ?? '')
    toast.success(t('common.copied'))
  } catch {
    // Clipboard access denied (insecure context / permission): surfaced as a
    // toast instead of leaving the click silently dead.
    toast.error(t('common.error'))
  }
}
</script>

<template>
  <div class="act-output">
    <p v-if="error" class="act-output-error" role="alert">
      <span class="act-output-error-text">{{ error }}</span>
      <button class="act-link-btn" type="button" @click="start">
        {{ t('activity.outputRetry') }}
      </button>
    </p>
    <template v-else>
      <div class="act-output-bar">
        <span v-if="tail?.truncated" class="act-output-note">
          {{
            t('activity.outputTruncated', {
              // Char count approximates the byte tail the API returned.
              shown: fmtBytes(tail.output.length),
              total: fmtBytes(tail.total_bytes),
            })
          }}
        </span>
        <button class="act-link-btn" type="button" :title="t('activity.outputCopy')" @click="copy">
          <Copy :size="12" aria-hidden="true" />
          {{ t('common.copy') }}
        </button>
      </div>
      <pre class="act-output-text">{{ tail?.output || t('activity.outputEmpty') }}</pre>
    </template>
  </div>
</template>

<style scoped>
.act-output {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-2) var(--space-3) var(--space-3);
  margin: var(--space-2) 0 0 var(--space-5);
  border-left: 2px solid var(--border);
}
.act-output-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}
.act-output-note {
  font-size: var(--text-xs);
  color: var(--muted);
}
.act-link-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: none;
  padding: 2px 4px;
  background: transparent;
  color: var(--muted);
  font: inherit;
  font-size: var(--text-xs);
  cursor: pointer;
  border-radius: var(--radius-xs);
}
.act-link-btn:hover {
  color: var(--text);
  background: var(--surface-hover);
}
.act-link-btn:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
}
.act-output-text {
  margin: 0;
  max-height: 220px;
  overflow-y: auto;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--text);
  white-space: pre-wrap;
  word-break: break-word;
}
.act-output-error {
  margin: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  font-size: var(--text-xs);
  color: var(--danger);
  overflow-wrap: anywhere;
}
</style>
