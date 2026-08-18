<script setup lang="ts">
import { computed, onMounted, ref, useTemplateRef, watch } from 'vue'
import { FileText } from 'lucide-vue-next'
import { fileIconBody, fileIconName, loadFileIcons, resolveViewBox } from '../lib/fileicons'

// Colorful per-extension file icon (VS Code icon theme). The collection is
// lazy-loaded as its own chunk; until it arrives (and for unknown types)
// the monochrome fallback keeps the tree layout stable.
//
// Icon paths don't match any fixed viewBox (they overflow their declared
// one), so the real content bounds are measured with getBBox() right after
// the body renders — before the browser paints, so nothing flickers — and
// cached per icon name in the fileicons module.
const props = withDefaults(defineProps<{ name: string; size?: number }>(), {
  size: 14,
})

const ready = ref(false)
onMounted(() => {
  void loadFileIcons().then(() => {
    ready.value = true
  })
})

// Depends on `ready` on purpose: `collection` inside fileIconBody() is a
// plain module variable (not reactive), so without a reactive trigger the
// computed would cache the null it sees before the icon chunk loads.
const icon = computed(() => {
  if (!ready.value) return null
  const name = fileIconName(props.name)
  return name ? fileIconBody(name) : null
})

const svg = useTemplateRef<SVGSVGElement>('svg')
const viewBox = ref('')

watch(
  () => [ready.value, icon.value] as const,
  () => {
    if (!ready.value || !icon.value) return
    if (!svg.value) return
    viewBox.value = resolveViewBox(
      icon.value.name,
      svg.value,
      icon.value.width,
      icon.value.height,
    )
  },
  // Run after the body has been injected into the DOM but before paint.
  { flush: 'post' },
)
</script>

<template>
  <!-- v-html is safe here: bodies come from the version-pinned local
       vscode-icons package, not from user input. -->
  <svg
    v-if="ready && icon"
    ref="svg"
    class="file-icon"
    :width="size"
    :height="size"
    :viewBox="viewBox"
    aria-hidden="true"
    v-html="icon.body"
  ></svg>
  <FileText v-else :size="size" />
</template>
