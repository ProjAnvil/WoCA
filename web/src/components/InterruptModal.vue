<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useSessionStore } from '../stores/sessions'
import { renderMarkdown } from '../lib/markdown'

const { t } = useI18n()
const store = useSessionStore()
const intr = computed(() => store.activeInterrupt)

// Selected answer per question index: option labels, or free text in [0].
const selected = ref<Record<number, string[]>>({})
watch(intr, () => {
  selected.value = {}
}, { immediate: true })

const planHtml = computed(() =>
  intr.value?.kind === 'plan' ? renderMarkdown(intr.value.payload.plan) : '',
)

function approve() {
  void store.answerInterrupt(true)
}
function deny() {
  void store.answerInterrupt(false)
}
function skip() {
  void store.answerInterrupt(true, [])
}
function submitAnswers() {
  const qs = intr.value?.kind === 'ask' ? intr.value.payload.questions : []
  const answers = qs.map((_, i) => (selected.value[i] ?? []).join(', '))
  void store.answerInterrupt(true, answers)
}
function toggleOption(qi: number, label: string, multi: boolean) {
  const cur = selected.value[qi] ?? []
  if (multi) {
    selected.value[qi] = cur.includes(label) ? cur.filter((l) => l !== label) : [...cur, label]
  } else {
    selected.value[qi] = [label]
  }
}
function onFreeText(qi: number, e: Event) {
  selected.value[qi] = [(e.target as HTMLInputElement).value]
}
</script>

<template>
  <Transition name="panel-pop">
    <div v-if="intr" class="overlay">
      <div class="card interrupt-card">
      <div class="card-head">
        <span class="card-title">
          <template v-if="intr.kind === 'permission'">{{ t('chat.permissionTitle') }}</template>
          <template v-else-if="intr.kind === 'plan'">{{ t('chat.planTitle') }}</template>
          <template v-else>{{ t('chat.inputTitle') }}</template>
        </span>
      </div>

      <div class="card-body">
        <!-- permission -->
        <template v-if="intr.kind === 'permission'">
          <p class="intr-reason">{{ intr.payload.reason }}</p>
          <div class="field-label">{{ t('chat.toolArgs') }}</div>
          <pre class="tool-args">{{ JSON.stringify(intr.payload.tool_args, null, 2) }}</pre>
        </template>

        <!-- plan -->
        <template v-else-if="intr.kind === 'plan'">
          <div class="markdown-body" v-html="planHtml"></div>
        </template>

        <!-- ask -->
        <template v-else-if="intr.kind === 'ask'">
          <div v-for="(q, qi) in intr.payload.questions" :key="qi" class="question-block">
            <div class="question-header">{{ q.header || t('chat.questionN', { index: qi + 1 }) }}</div>
            <div class="question-text">{{ q.question }}</div>

            <div v-if="q.options && q.options.length" class="option-list">
              <label v-for="(opt, oi) in q.options" :key="oi" class="option-row">
                <input
                  :type="q.multi_select ? 'checkbox' : 'radio'"
                  :name="`q${qi}`"
                  :checked="(selected[qi] ?? []).includes(opt.label)"
                  @change="toggleOption(qi, opt.label, !!q.multi_select)"
                />
                <span class="option-label">{{ opt.label }}</span>
                <span v-if="opt.description" class="option-desc">{{ opt.description }}</span>
              </label>
            </div>
            <input
              v-else
              class="field"
              :value="(selected[qi] ?? [])[0] ?? ''"
              :placeholder="t('chat.answerPlaceholder')"
              @input="onFreeText(qi, $event)"
            />
          </div>
        </template>
      </div>

      <div class="interrupt-actions">
        <template v-if="intr.kind === 'ask'">
          <button class="btn" @click="skip">{{ t('chat.skip') }}</button>
          <button class="btn btn-primary" @click="submitAnswers">{{ t('chat.submit') }}</button>
        </template>
        <template v-else>
          <button class="btn" @click="deny">{{ t('chat.deny') }}</button>
          <button class="btn btn-primary" @click="approve">{{ t('chat.allow') }}</button>
        </template>
      </div>
      </div>
    </div>
  </Transition>
</template>
