<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import WSheet from './ui/WSheet.vue'
import { Pause, Play, Target, Trash2, X } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'
import { fmtTokens } from '../lib/format'
import type { GoalStatus } from '../types'

const { t } = useI18n()
const store = useSessionStore()

const objective = ref('')
const cycles = ref<number | null>(null)
const tokens = ref<number | null>(null)

const goal = computed(() => store.activeGoal)

const statusLabels = computed<Record<GoalStatus, string>>(() => ({
  active: t('goal.status.active'),
  paused: t('goal.status.paused'),
  blocked: t('goal.status.blocked'),
  complete: t('goal.status.complete'),
  budget_limited: t('goal.status.budgetLimited'),
}))

function pct(used: number, max: number): number {
  if (max <= 0) return 0
  return Math.min(100, Math.round((used / max) * 100))
}

function close() {
  store.goalPanelOpen = false
}

async function run(command: string) {
  await store.sendGoalCommand(command)
}

async function setGoal() {
  const obj = objective.value.trim()
  if (!obj) return
  const args = ['set']
  if (cycles.value && cycles.value > 0) args.push('--cycles', String(cycles.value))
  if (tokens.value && tokens.value > 0) args.push('--tokens', String(tokens.value))
  await store.sendGoalCommand(`${args.join(' ')} ${obj}`)
  objective.value = ''
  cycles.value = null
  tokens.value = null
}
</script>

<template>
  <WSheet size="md" @close="close()">
    <div class="goal-card">
      <div class="card-head">
        <span class="card-title goal-title">
          <Target :size="15" />
          {{ t('goal.title') }}
        </span>
        <button class="icon-btn" @click="close()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body goal-body">
        <!-- No goal yet: the set form -->
        <div v-if="!goal" class="goal-empty">
          <p class="goal-hint">
            {{ t('goal.hint') }}
            <code>/goal {{ t('goal.hintObjective') }}</code>{{ t('goal.hintSuffix') }}
          </p>
          <textarea
            v-model="objective"
            class="goal-objective-input"
            rows="3"
            :placeholder="t('goal.objectivePlaceholder')"
            @keydown.enter.exact.prevent="setGoal()"
          ></textarea>
          <div class="goal-budget-row">
            <label>
              {{ t('goal.maxCycles') }}
              <input v-model.number="cycles" type="number" min="1" placeholder="10" />
            </label>
            <label>
              {{ t('goal.tokenBudget') }}
              <input v-model.number="tokens" type="number" min="1000" placeholder="2000000" />
            </label>
            <button class="goal-btn primary" :disabled="!objective.trim()" @click="setGoal()">
              {{ t('goal.start') }}
            </button>
          </div>
        </div>

        <!-- Active/past goal: status + controls -->
        <div v-else class="goal-detail">
          <div class="goal-objective">{{ goal.objective }}</div>
          <div class="goal-meta">
            <span class="goal-status" :class="`s-${goal.status}`">{{ statusLabels[goal.status] }}</span>
            <span v-if="goal.claimed" class="goal-claim">{{ t('goal.claimPending') }}</span>
            <span v-if="store.goalRunning[store.activeId]" class="goal-live">{{ t('goal.running') }}</span>
          </div>

          <div class="goal-budgets">
            <div class="goal-meter">
              <div class="goal-meter-label">
                <span>{{ t('goal.cycles') }}</span><span>{{ goal.cycles }} / {{ goal.max_cycles }}</span>
              </div>
              <div class="goal-meter-bar">
                <div class="goal-meter-fill" :style="{ width: pct(goal.cycles, goal.max_cycles) + '%' }"></div>
              </div>
            </div>
            <div class="goal-meter">
              <div class="goal-meter-label">
                <span>Tokens</span><span>{{ fmtTokens(goal.tokens_used) }} / {{ fmtTokens(goal.max_tokens) }}</span>
              </div>
              <div class="goal-meter-bar">
                <div class="goal-meter-fill tokens" :style="{ width: pct(goal.tokens_used, goal.max_tokens) + '%' }"></div>
              </div>
            </div>
          </div>

          <p v-if="goal.status === 'blocked' && goal.blocked_reason" class="goal-note danger">
            {{ t('goal.blocked', { reason: goal.blocked_reason }) }}
          </p>
          <p v-else-if="goal.status === 'complete' && goal.evidence" class="goal-note ok">
            {{ t('goal.evidence', { evidence: goal.evidence }) }}
          </p>
          <p v-if="goal.claimed && goal.claim_evidence" class="goal-note">
            {{ t('goal.claim', { evidence: goal.claim_evidence }) }}
          </p>
          <p v-if="goal.notes?.length" class="goal-note">
            {{ t('goal.latestNote', { note: goal.notes[goal.notes.length - 1] }) }}
          </p>

          <div v-if="goal.progress?.length" class="goal-progress">
            <div v-for="p in [...goal.progress].reverse()" :key="p.cycle" class="goal-progress-row">
              <span class="goal-cycle">C{{ p.cycle }}</span>
              <span class="goal-verdict" :class="`v-${p.verdict}`">{{ p.verdict }}</span>
              <span class="goal-summary">{{ p.summary || p.note }}</span>
            </div>
          </div>

          <div class="goal-actions">
            <button
              v-if="goal.status === 'active'"
              class="goal-btn"
              @click="run('pause')"
            >
              <Pause :size="13" /> {{ t('goal.pause') }}
            </button>
            <button
              v-if="goal.status !== 'active' && goal.status !== 'complete'"
              class="goal-btn primary"
              @click="run('resume')"
            >
              <Play :size="13" /> {{ t('goal.resume') }}
            </button>
            <button class="goal-btn danger" @click="run('clear')">
              <Trash2 :size="13" /> {{ t('common.clear') }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </WSheet>
</template>
