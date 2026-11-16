<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ApprovalAsk, InterruptCardData } from '../../types'
import { useSessionStore } from '../../stores/sessions'
import { toErrorMessage } from '../../lib/errors'
import { fmtDuration } from '../../lib/format'
import { askCardAnswer, asksOf, cardAnswer, hitlCardKey, hitlOrigin, hitlRequestId, hitlShortCode, riskTier, sessionRuleKeyOf } from '../../lib/hitl'
import OriginBadge from '../activity/OriginBadge.vue'

// One approval card of the HITL stack (§4.2): origin badge + 4-char call-id
// short code, tool + collapsed args line, a 2px risk rail, and the three
// buttons — allow once / allow for session (previewing the rule key it will
// create) / deny. Per-card state machine: pending → answering → resolved,
// or error (inline alert + retry).
const props = defineProps<{
  card: InterruptCardData
  /** Shared elapsed tick from the stack (one interval for all cards). */
  now: number
}>()

const { t } = useI18n()
const store = useSessionStore()

// Card park time (the elapsed label's zero point; the stack owns the tick).
const mountedAt = Date.now()

const asks = computed(() => asksOf(props.card))
const requestId = computed(() => hitlRequestId(props.card))
const key = computed(() => hitlCardKey(props.card))
const origin = computed(() => hitlOrigin(props.card))
const shortCode = computed(() => hitlShortCode(props.card))
const ruleKey = computed(() => sessionRuleKeyOf(props.card))
const isAsk = computed(() => props.card.kind === 'ask')
const isPlan = computed(() => props.card.kind === 'plan')

// The flat payload fields (legacy permission cards and single-ask envelopes).
const flat = computed(() => {
  const p = props.card.payload
  return typeof p === 'object' && p !== null ? (p as Record<string, unknown>) : {}
})
const primaryAsk = computed<ApprovalAsk | null>(() => (asks.value.length === 1 ? asks.value[0] : null))
const toolName = computed(
  () => primaryAsk.value?.tool_name || (typeof flat.value.tool_name === 'string' ? flat.value.tool_name : (isAsk.value ? 'AskUserQuestion' : isPlan.value ? 'ExitPlanMode' : props.card.kind)),
)
const toolArgs = computed<Record<string, unknown> | undefined>(() => primaryAsk.value?.tool_args ?? (flat.value.tool_args as Record<string, unknown> | undefined))
const isReadOnly = computed(() => flat.value.is_read_only === true)
const tier = computed(() => riskTier(toolName.value, toolArgs.value, isReadOnly.value))
const reason = computed(() => primaryAsk.value?.reason || (typeof flat.value.reason === 'string' ? flat.value.reason : ''))
const planText = computed(() => {
  if (!isPlan.value) return ''
  const p = primaryAsk.value?.plan
  if (typeof p === 'string' && p) return p
  return typeof flat.value.plan === 'string' ? flat.value.plan : ''
})
const questions = computed(() => {
  if (!isAsk.value) return []
  const q = primaryAsk.value?.questions ?? flat.value.questions
  return Array.isArray(q) ? q : []
})

// One-line args summary; JSON over 2KB collapses (title keeps the full text).
const ARGS_LIMIT = 2048
const argsFull = computed(() => {
  const summary = primaryAsk.value?.args_summary
  if (summary) return summary
  try {
    return JSON.stringify(toolArgs.value ?? {}, null, 2)
  } catch {
    return ''
  }
})
const argsLine = computed(() => (argsFull.value.length > ARGS_LIMIT ? `${argsFull.value.slice(0, ARGS_LIMIT)}…` : argsFull.value))

// --- answering ---
type Phase = 'pending' | 'answering' | 'resolved' | 'error'
const phase = ref<Phase>('pending')
const errMsg = ref('')
const busy = computed(() => phase.value === 'answering' || props.card.timedOut === true)

async function answer(outcome: 'allow' | 'deny', grant?: 'session') {
  if (busy.value || phase.value === 'resolved') return
  phase.value = 'answering'
  errMsg.value = ''
  try {
    await store.answerCards([cardAnswer(props.card, outcome, grant)])
    phase.value = 'resolved'
  } catch (e) {
    // Inline alert + retry: the buttons come back, the parked card stays.
    phase.value = 'error'
    errMsg.value = toErrorMessage(e)
  }
}

// Ask-card selections (option labels, or free text) per question index.
const selected = ref<Record<number, string[]>>({})
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
async function submitAsk() {
  const answers = questions.value.map((_, i) => (selected.value[i] ?? []).join(', '))
  phase.value = 'answering'
  try {
    await store.answerCards([askCardAnswer(props.card, answers)])
    phase.value = 'resolved'
  } catch (e) {
    phase.value = 'error'
    errMsg.value = toErrorMessage(e)
  }
}
async function cancelAsk() {
  // Per-card cancel ruling (T8): a cancelled ask is an empty answer entry.
  phase.value = 'answering'
  try {
    await store.answerCards([askCardAnswer(props.card, [])])
    phase.value = 'resolved'
  } catch (e) {
    phase.value = 'error'
    errMsg.value = toErrorMessage(e)
  }
}

const elapsed = computed(() => fmtDuration(props.now - mountedAt))

const actionLabel = (what: string) => `${what} · ${origin.value} · ${toolName.value}`
</script>

<template>
  <div
    class="hitl-card interrupt-card card"
    :class="[`tier-${tier}`, { 'is-timed-out': card.timedOut }]"
    role="group"
    tabindex="-1"
    :aria-labelledby="`hitl-tool-${requestId}`"
    :data-key="key"
  >
    <div class="hitl-card-head">
      <OriginBadge :origin="origin" />
      <span class="hitl-short-code" :title="requestId">#{{ shortCode }}</span>
      <span class="hitl-elapsed">{{ elapsed }}</span>
    </div>

    <div class="hitl-card-body">
      <!-- ask (AskUserQuestion) card -->
      <template v-if="isAsk">
        <div v-for="(q, qi) in questions" :key="qi" class="question-block">
          <div class="question-header" :id="qi === 0 ? `hitl-tool-${requestId}` : undefined">{{ q.header || t('chat.questionN', { index: qi + 1 }) }}</div>
          <div class="question-text">{{ q.question }}</div>
          <div v-if="q.options && q.options.length" class="option-list">
            <label v-for="(opt, oi) in q.options" :key="oi" class="option-row">
              <input
                :type="q.multi_select ? 'checkbox' : 'radio'"
                :name="`h${requestId}-q${qi}`"
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

      <!-- plan card -->
      <template v-else-if="isPlan">
        <p class="intr-reason" :id="`hitl-tool-${requestId}`">{{ reason }}</p>
        <pre class="hitl-plan">{{ planText }}</pre>
      </template>

      <!-- permission / approvals card -->
      <template v-else>
        <p v-if="reason" class="intr-reason">{{ reason }}</p>
        <!-- single ask (or legacy flat card): the classic one-tool card -->
        <div v-if="primaryAsk || asks.length === 0" class="hitl-tool-line">
          <span :id="`hitl-tool-${requestId}`" class="hitl-tool-name">{{ toolName }}</span>
          <span v-if="argsLine" class="hitl-tool-args" :title="argsFull">{{ argsLine }}</span>
        </div>
        <!-- multi-ask envelope: one row per ask, buttons fan out -->
        <div v-else class="hitl-ask-rows" role="list">
          <div v-for="a in asks" :key="a.call_id" class="hitl-tool-line" role="listitem">
            <span class="hitl-tool-name">{{ a.tool_name }}</span>
            <span class="hitl-tool-args" :title="a.args_summary || JSON.stringify(a.tool_args ?? {})">{{
              (a.args_summary || JSON.stringify(a.tool_args ?? {})).slice(0, ARGS_LIMIT)
            }}</span>
          </div>
        </div>
      </template>
    </div>

    <div v-if="card.timedOut" class="hitl-card-state hitl-timed-out">{{ t('chat.requestTimedOut') }}</div>
    <div v-else-if="errMsg" class="hitl-card-state hitl-error" role="alert">
      {{ errMsg }} · <button class="hitl-retry" type="button" @click="phase = 'pending'">{{ t('common.retry') }}</button>
    </div>

    <div v-if="!card.timedOut" class="interrupt-actions">
      <!-- ask cards: submit / cancel -->
      <template v-if="isAsk">
        <button class="btn" type="button" :disabled="busy" @click="cancelAsk">{{ t('chat.requestCancelled') }}</button>
        <button class="btn btn-primary" type="button" :disabled="busy" @click="submitAsk">{{ t('chat.submit') }}</button>
      </template>
      <!-- permission / plan / approvals cards -->
      <template v-else>
        <button
          class="btn"
          type="button"
          :disabled="busy"
          :data-testid="`hitl-deny-${requestId}`"
          :aria-label="actionLabel(t('chat.denyRequest'))"
          @click="answer('deny')"
        >
          {{ t('chat.denyRequest') }}
        </button>
        <button
          v-if="ruleKey"
          class="btn"
          type="button"
          :disabled="busy"
          :data-testid="`hitl-grant-session-${requestId}`"
          :aria-label="actionLabel(t('chat.approveSession'))"
          @click="answer('allow', 'session')"
        >
          {{ t('chat.approveSession') }}
          <span class="hitl-rule-key" :data-testid="`hitl-rule-key-${requestId}`" :title="t('chat.sessionRuleKey', { key: ruleKey })">{{
            ruleKey
          }}</span>
        </button>
        <button
          class="btn btn-primary"
          type="button"
          :disabled="busy"
          :data-testid="`hitl-allow-${requestId}`"
          :aria-label="actionLabel(t('chat.approveOnce'))"
          @click="answer('allow')"
        >
          {{ t('chat.approveOnce') }}
        </button>
      </template>
    </div>
  </div>
</template>

<style scoped>
.hitl-card {
  position: relative;
  border-left: 2px solid var(--border);
}
/* The risk rail (act-row precedent): color is the signal, never the only one. */
.hitl-card.tier-neutral {
  border-left-color: var(--muted);
}
.hitl-card.tier-warn {
  border-left-color: var(--warn);
}
.hitl-card.tier-danger {
  border-left-color: var(--danger);
}
.hitl-card.is-timed-out {
  opacity: 0.7;
}
.hitl-card-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 12px 20px 0;
}
.hitl-short-code {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--muted);
}
.hitl-elapsed {
  margin-inline-start: auto;
  font-size: var(--text-xs);
  color: var(--muted);
  font-variant-numeric: tabular-nums;
}
.hitl-card-body {
  padding: 10px 20px;
}
.hitl-tool-line {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}
.hitl-tool-name {
  font-weight: 600;
  flex-shrink: 0;
}
.hitl-tool-args {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--muted);
}
.hitl-ask-rows {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.hitl-plan {
  margin: 0;
  max-height: 40vh;
  overflow: auto;
  white-space: pre-wrap;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}
.hitl-card-state {
  padding: 6px 20px;
  font-size: var(--text-xs);
}
.hitl-timed-out {
  color: var(--muted);
}
.hitl-error {
  color: var(--danger-text);
}
.hitl-retry {
  background: none;
  border: none;
  padding: 0;
  color: var(--link);
  text-decoration: underline;
  cursor: pointer;
  font: inherit;
}
/* The session button inlines the rule key it will create (mono on warn-soft). */
.hitl-rule-key {
  margin-inline-start: 6px;
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  background: var(--warn-soft);
  color: var(--warn-text);
  font-family: var(--font-mono);
  font-size: 11px;
}
</style>
