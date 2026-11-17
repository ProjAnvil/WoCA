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

// Args body: a wrapping `pre` (pre-wrap + overflow-wrap anywhere) keeps every
// character reachable on any viewport; over 2KB the tail is cut (payload
// safety). Collapsed it caps at ARGS_COLLAPSED_LINES until the toggle
// expands it — the toggle only appears when the wrapped-line estimate
// (28 mono chars/line) exceeds the cap. The estimate is deliberately
// conservative: under-estimating capacity merely shows a harmless no-op
// toggle, while over-estimating it would clip text with no toggle.
const ARGS_LIMIT = 2048
const ARGS_COLLAPSED_LINES = 8
const ARGS_CHARS_PER_LINE = 28

function safeJsonArgs(v: unknown): string {
  try {
    return JSON.stringify(v ?? {}, null, 2)
  } catch {
    return ''
  }
}
function capArgs(text: string): string {
  return text.length > ARGS_LIMIT ? `${text.slice(0, ARGS_LIMIT)}…` : text
}
function estimateLines(text: string): number {
  return text.split('\n').reduce((n, line) => n + Math.max(1, Math.ceil(line.length / ARGS_CHARS_PER_LINE)), 0)
}

const argsText = computed(() => capArgs(primaryAsk.value?.args_summary || safeJsonArgs(toolArgs.value)))
const argsOverflows = computed(() => estimateLines(argsText.value) > ARGS_COLLAPSED_LINES)
const argsOpen = ref(false)

function askArgsText(a: ApprovalAsk): string {
  return capArgs(a.args_summary || safeJsonArgs(a.tool_args))
}
function askOverflows(a: ApprovalAsk): boolean {
  return estimateLines(askArgsText(a)) > ARGS_COLLAPSED_LINES
}
const askArgsOpen = ref<Record<string, boolean>>({})

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
        <div v-if="primaryAsk || asks.length === 0" class="hitl-tool">
          <span :id="`hitl-tool-${requestId}`" class="hitl-tool-name">{{ toolName }}</span>
          <pre v-if="argsText" class="hitl-tool-args" :class="{ 'is-collapsed': !argsOpen }">{{ argsText }}</pre>
          <button
            v-if="argsOverflows"
            class="hitl-args-toggle"
            type="button"
            :data-testid="`hitl-args-toggle-${requestId}`"
            :aria-expanded="argsOpen ? 'true' : 'false'"
            @click="argsOpen = !argsOpen"
          >
            {{ argsOpen ? t('chat.collapseArgs') : t('chat.expandArgs') }}
          </button>
        </div>
        <!-- multi-ask envelope: one block per ask, buttons fan out -->
        <div v-else class="hitl-ask-rows" role="list">
          <div v-for="a in asks" :key="a.call_id" class="hitl-tool" role="listitem">
            <span class="hitl-tool-name">{{ a.tool_name }}</span>
            <pre v-if="askArgsText(a)" class="hitl-tool-args" :class="{ 'is-collapsed': !askArgsOpen[a.call_id] }">{{ askArgsText(a) }}</pre>
            <button
              v-if="askOverflows(a)"
              class="hitl-args-toggle"
              type="button"
              :data-testid="`hitl-args-toggle-${a.call_id}`"
              :aria-expanded="askArgsOpen[a.call_id] ? 'true' : 'false'"
              @click="askArgsOpen[a.call_id] = !askArgsOpen[a.call_id]"
            >
              {{ askArgsOpen[a.call_id] ? t('chat.collapseArgs') : t('chat.expandArgs') }}
            </button>
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
      <!-- permission / plan / approvals cards: deny = outlined secondary,
           allow-for-session = ghost, allow-once = filled primary. -->
      <template v-else>
        <button
          class="btn hitl-btn-deny"
          type="button"
          :disabled="busy"
          :data-testid="`hitl-deny-${requestId}`"
          :aria-label="actionLabel(t('chat.denyRequest'))"
          @click="answer('deny')"
        >
          {{ t('chat.denyRequest') }}
        </button>
        <!-- The rule key a session answer creates: its own readable line
             directly above the session button it belongs to (never a chip
             inside the button). -->
        <span
          v-if="ruleKey"
          class="hitl-rule-key"
          :data-testid="`hitl-rule-key-${requestId}`"
          :title="t('chat.sessionRuleKey', { key: ruleKey })"
        >{{ ruleKey }}</span>
        <button
          v-if="ruleKey"
          class="btn hitl-btn-ghost hitl-btn-session"
          type="button"
          :disabled="busy"
          :data-testid="`hitl-grant-session-${requestId}`"
          :aria-label="actionLabel(t('chat.approveSession'))"
          @click="answer('allow', 'session')"
        >
          {{ t('chat.approveSession') }}
        </button>
        <button
          class="btn btn-primary hitl-btn-allow"
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
.hitl-tool {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-2);
  min-width: 0;
}
.hitl-tool-name {
  font-weight: 600;
  flex-shrink: 0;
}
/* Args body: pre-wrap + overflow-wrap anywhere keeps every character
 * reachable at any viewport; collapsed it caps at 8 lines (lh-xs scale)
 * until the toggle beneath expands it. */
.hitl-tool-args {
  margin: 0;
  width: 100%;
  box-sizing: border-box;
  padding: var(--space-3);
  background: var(--surface-2);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--text-2);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  overflow: hidden;
}
.hitl-tool-args.is-collapsed {
  max-height: calc(var(--lh-xs) * 8 + var(--space-3) * 2);
}
.hitl-args-toggle {
  background: none;
  border: none;
  padding: 0;
  font-size: var(--text-xs);
  color: var(--link);
  text-decoration: underline;
  cursor: pointer;
}
.hitl-args-toggle:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 1px;
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
/* Rule key: its own readable line (mono, wraps anywhere) directly above
 * the session button it belongs to — no longer a chip inside the button. */
.hitl-rule-key {
  flex-basis: 100%;
  text-align: end;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--warn-text);
  overflow-wrap: anywhere;
}
/* Allow-for-session = ghost tier (WButton's w-btn--ghost grammar): the
 * filled primary stays with allow-once, deny keeps the outlined default. */
.hitl-btn-ghost {
  background: transparent;
  border-color: transparent;
  color: var(--text-2);
}
.hitl-btn-ghost:hover {
  background: var(--surface-hover);
  color: var(--text);
}
/* ≤480: the row stacks — deny full-width on its own line above, the two
 * allow actions share the line below (the rule-key line sits between). */
@media (max-width: 480px) {
  .hitl-btn-deny {
    flex-basis: 100%;
  }
  .hitl-btn-session,
  .hitl-btn-allow {
    flex: 1 1 0;
    min-width: 0;
  }
}
</style>
