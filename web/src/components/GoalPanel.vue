<script setup lang="ts">
import { computed, ref } from 'vue'
import { Pause, Play, Target, Trash2, X } from 'lucide-vue-next'
import { useSessionStore } from '../stores/sessions'
import type { GoalStatus } from '../types'

const store = useSessionStore()

const objective = ref('')
const cycles = ref<number | null>(null)
const tokens = ref<number | null>(null)

const goal = computed(() => store.activeGoal)

const statusLabels: Record<GoalStatus, string> = {
  active: '进行中',
  paused: '已暂停',
  blocked: '受阻',
  complete: '已完成',
  budget_limited: '预算耗尽',
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`
  return String(n)
}

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
  <div class="overlay" @click.self="close()">
    <div class="card goal-card">
      <div class="card-head">
        <span class="card-title goal-title">
          <Target :size="15" />
          目标模式
        </span>
        <button class="icon-btn" @click="close()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body goal-body">
        <!-- No goal yet: the set form -->
        <div v-if="!goal" class="goal-empty">
          <p class="goal-hint">
            设定一个持久目标，woca 会以「评估 → 执行 → 验证」循环自主推进，
            直到目标被独立评估器验证完成、受阻或预算耗尽。对话里也可以直接输入
            <code>/goal set &lt;目标&gt;</code>。
          </p>
          <textarea
            v-model="objective"
            class="goal-objective-input"
            rows="3"
            placeholder="目标，例如：给 internal/server 补齐 goal API 的集成测试并全部跑绿"
            @keydown.enter.exact.prevent="setGoal()"
          ></textarea>
          <div class="goal-budget-row">
            <label>
              最大循环
              <input v-model.number="cycles" type="number" min="1" placeholder="10" />
            </label>
            <label>
              Token 预算
              <input v-model.number="tokens" type="number" min="1000" placeholder="2000000" />
            </label>
            <button class="goal-btn primary" :disabled="!objective.trim()" @click="setGoal()">
              启动目标循环
            </button>
          </div>
        </div>

        <!-- Active/past goal: status + controls -->
        <div v-else class="goal-detail">
          <div class="goal-objective">{{ goal.objective }}</div>
          <div class="goal-meta">
            <span class="goal-status" :class="`s-${goal.status}`">{{ statusLabels[goal.status] }}</span>
            <span v-if="goal.claimed" class="goal-claim">完成声明待验证</span>
            <span v-if="store.goalRunning[store.activeId]" class="goal-live">循环运行中…</span>
          </div>

          <div class="goal-budgets">
            <div class="goal-meter">
              <div class="goal-meter-label">
                <span>循环</span><span>{{ goal.cycles }} / {{ goal.max_cycles }}</span>
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
            受阻：{{ goal.blocked_reason }}
          </p>
          <p v-else-if="goal.status === 'complete' && goal.evidence" class="goal-note ok">
            验证依据：{{ goal.evidence }}
          </p>
          <p v-if="goal.claimed && goal.claim_evidence" class="goal-note">
            执行者声明：{{ goal.claim_evidence }}
          </p>
          <p v-if="goal.notes?.length" class="goal-note">
            最近记录：{{ goal.notes[goal.notes.length - 1] }}
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
              <Pause :size="13" /> 暂停
            </button>
            <button
              v-if="goal.status !== 'active' && goal.status !== 'complete'"
              class="goal-btn primary"
              @click="run('resume')"
            >
              <Play :size="13" /> 继续
            </button>
            <button class="goal-btn danger" @click="run('clear')">
              <Trash2 :size="13" /> 清除
            </button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
