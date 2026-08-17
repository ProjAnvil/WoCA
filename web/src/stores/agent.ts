import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { PermissionMode, QuotaResponse, ThinkingLevel } from '../types'
import * as api from '../api/client'
import { fmtTokens } from '../lib/format'

/** 中文标签：权限模式 / 思考级别 / 额度窗口。 */
export const MODE_LABELS: Record<string, string> = {
  default: '默认',
  acceptEdits: '自动接受编辑',
  bypassPermissions: '跳过权限',
  readOnly: '只读',
  plan: '计划',
}

export const MODE_DESCRIPTIONS: Record<string, string> = {
  default: '写入与命令执行前逐个确认',
  acceptEdits: '文件编辑免确认，命令仍确认',
  bypassPermissions: '所有工具直接放行',
  readOnly: '禁止一切写入与命令执行',
  plan: '只读调研，先出计划再执行',
}

export const THINKING_LABELS: Record<string, string> = {
  off: '关',
  low: '低',
  medium: '中',
  high: '高',
}

export const THINKING_LEVELS: ThinkingLevel[] = ['off', 'low', 'medium', 'high']

export const THINKING_BUDGETS: Record<string, number> = {
  off: 0,
  low: 4096,
  medium: 10240,
  high: 31999,
}

const WINDOW_LABELS: Record<string, string> = {
  '5h': '5 小时窗口',
  weekly: '每周额度',
  mcp: 'MCP 工具',
}

function resetTime(ms: number): string {
  if (!ms) return ''
  const d = new Date(ms)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return `${hh}:${mm} 重置`
}

/**
 * Runtime agent controls behind the chat-input toolbar: the permission-mode
 * and thinking-level selectors and the quota chip. Backed by /api/agent and
 * /api/quota.
 */
export const useAgentStore = defineStore('agent', () => {
  const state = ref<{
    provider: string
    model: string
    permission_mode: string
    plan_mode: boolean
    thinking: { level: string; budget: number; supported: boolean; levels: string[]; note?: string }
  } | null>(null)
  const quota = ref<QuotaResponse | null>(null)
  const quotaLoading = ref(false)
  const quotaError = ref('')

  const mode = computed<PermissionMode | string>(() => state.value?.permission_mode ?? 'default')
  const modeLabel = computed(() => MODE_LABELS[mode.value] ?? mode.value)
  const thinkingLevel = computed(() => state.value?.thinking.level ?? 'off')
  const thinkingLabel = computed(() => THINKING_LABELS[thinkingLevel.value] ?? thinkingLevel.value)
  const thinkingBudget = computed(() => state.value?.thinking.budget ?? 0)
  // Provider-template capability: models without a thinking mode keep the
  // drawer closed (the chip renders disabled).
  const thinkingSupported = computed(() => state.value?.thinking.supported ?? false)
  const thinkingLevels = computed<string[]>(() => state.value?.thinking.levels ?? [])
  const thinkingNote = computed(() => state.value?.thinking.note ?? '')

  // 额度 chip 的短摘要：余额型直接展示金额；套餐型优先 5h 窗口剩余。
  const quotaShort = computed(() => {
    const q = quota.value?.quota
    if (!q?.available) return '额度 —'
    if (q.type === 'balance' && q.balance) return `余额 ${q.balance}`
    const parts: string[] = []
    for (const kind of ['5h', 'weekly']) {
      const w = q.windows?.find((x) => x.kind === kind)
      if (w) parts.push(`${kind === '5h' ? '5h' : '周'} 剩 ${Math.max(0, 100 - w.used_percent).toFixed(0)}%`)
    }
    return parts.join(' · ') || '套餐额度'
  })

  // /quota 命令与额度弹层的多行文本。
  const quotaText = computed(() => {
    const q = quota.value
    if (!q) return '额度信息未加载。'
    const lines: string[] = []
    if (q.quota.available) {
      if (q.quota.type === 'balance') {
        lines.push(`账户余额：${q.quota.balance ?? '未知'}`)
      } else {
        lines.push(
          q.quota.level ? `套餐（${q.quota.level}）剩余额度：` : '套餐剩余额度：',
        )
        for (const w of q.quota.windows ?? []) {
          const remaining = Math.max(0, 100 - w.used_percent)
          const reset = w.next_reset_ms ? `，${resetTime(w.next_reset_ms)}` : ''
          lines.push(`  ${WINDOW_LABELS[w.kind] ?? w.kind}：剩余 ${remaining.toFixed(1)}%（已用 ${w.used_percent.toFixed(1)}%${reset}）`)
        }
      }
    } else {
      lines.push(`额度不可用：${q.quota.reason ?? '未知原因'}`)
    }
    const s = q.session
    if (s?.requests) {
      const cost =
        s.cost_usd != null && s.cost_known ? `，估算 $${s.cost_usd.toFixed(2)}` : ''
      lines.push(
        `本会话用量：${s.requests} 次请求 · 输入 ${fmtTokens(s.input_tokens ?? 0)} / 输出 ${fmtTokens(s.output_tokens ?? 0)} tokens${cost}`,
      )
    }
    return lines.join('\n')
  })

  async function load() {
    try {
      state.value = await api.getAgent()
    } catch {
      state.value = null // 工具条降级为占位显示
    }
  }

  async function setMode(m: string) {
    if (!MODE_LABELS[m]) throw new Error(`未知权限模式 ${m}`)
    await api.setPermissionMode(m)
    if (state.value) state.value.permission_mode = m
  }

  async function setThinking(level: string) {
    if (!THINKING_LABELS[level]) throw new Error(`未知思考级别 ${level}`)
    if (level !== 'off' && thinkingLevels.value.length && !thinkingLevels.value.includes(level)) {
      throw new Error(`当前模型不支持思考级别 ${level}`)
    }
    const resp = await api.setThinkingLevel(level)
    if (state.value) {
      state.value.thinking = {
        ...state.value.thinking,
        level: resp.level,
        budget: resp.budget,
      }
    } else {
      state.value = {
        provider: '',
        model: '',
        permission_mode: 'default',
        plan_mode: false,
        thinking: { level: resp.level, budget: resp.budget, supported: true, levels: [] },
      }
    }
  }

  async function refreshQuota() {
    quotaLoading.value = true
    quotaError.value = ''
    try {
      quota.value = await api.getQuota()
    } catch (e) {
      quotaError.value = e instanceof Error ? e.message : String(e)
    } finally {
      quotaLoading.value = false
    }
  }

  return {
    state,
    quota,
    quotaLoading,
    quotaError,
    mode,
    modeLabel,
    thinkingLevel,
    thinkingLabel,
    thinkingBudget,
    thinkingSupported,
    thinkingLevels,
    thinkingNote,
    quotaShort,
    quotaText,
    load,
    setMode,
    setThinking,
    refreshQuota,
  }
})
