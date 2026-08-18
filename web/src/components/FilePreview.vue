<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { CircleAlert } from 'lucide-vue-next'
import { useWorkspaceStore } from '../stores/workspace'
import { renderMarkdown } from '../lib/markdown'
import { highlightCode, langForFile } from '../lib/highlight'
import FileIcon from './FileIcon.vue'

const { t } = useI18n()
const store = useWorkspaceStore()

const file = computed(() => store.preview)

const isMarkdown = computed(() => /\.(md|mdx|markdown)$/i.test(file.value?.name ?? ''))

const mdHtml = computed(() =>
  isMarkdown.value ? renderMarkdown(file.value?.content ?? '') : '',
)

// Shiki highlights asynchronously; swap in the highlighted HTML when ready.
const codeHtml = ref<string | null>(null)
watch(
  () => [file.value?.path, file.value?.content],
  async () => {
    const f = file.value
    codeHtml.value = null
    if (!f || isMarkdown.value) return
    const lang = langForFile(f.name)
    if (lang === 'text') return
    codeHtml.value = await highlightCode(f.content ?? '', lang)
  },
  { immediate: true },
)

// Plain fallback for text without a grammar.
const plainText = computed(() =>
  !isMarkdown.value && !codeHtml.value ? (file.value?.content ?? '') : '',
)
</script>

<template>
  <div class="file-preview">
    <div class="fp-head">
      <FileIcon :name="file?.name ?? ''" :size="14" class="fp-file-icon" />
      <span class="fp-title" :title="file?.path">{{ file?.name }}</span>
    </div>

    <div v-if="store.previewLoading" class="fp-state">{{ t('common.loading') }}</div>
    <div v-else-if="store.previewError" class="fp-state fp-error">
      <CircleAlert :size="14" />
      {{ store.previewError }}
    </div>

    <div v-else class="fp-body">
      <div v-if="isMarkdown" class="markdown-body" v-html="mdHtml"></div>
      <div
        v-else-if="codeHtml"
        class="code-view shiki-host"
        v-html="codeHtml"
      ></div>
      <pre v-else class="fp-plain">{{ plainText }}</pre>
    </div>
  </div>
</template>
