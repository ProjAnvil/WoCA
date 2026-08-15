<script setup lang="ts">
import { computed } from 'vue'
import { FileText, X } from 'lucide-vue-next'
import { useWorkspaceStore } from '../stores/workspace'
import { renderMarkdown } from '../lib/markdown'

const store = useWorkspaceStore()

const html = computed(() => (store.preview ? renderMarkdown(store.preview.content ?? '') : ''))
</script>

<template>
  <div class="overlay" @click.self="store.closePreview()">
    <div class="card preview-card">
      <div class="card-head">
        <div class="preview-title">
          <FileText :size="16" />
          <span class="card-title">{{ store.preview?.name }}</span>
        </div>
        <button class="icon-btn" @click="store.closePreview()">
          <X :size="16" />
        </button>
      </div>
      <div class="preview-body">
        <div class="markdown-body" v-html="html"></div>
      </div>
    </div>
  </div>
</template>
