import { toErrorMessage } from '../lib/errors'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { PermissionMode, QuotaResponse, ThinkingLevel } from '../types'
import * as api from '../api/client'
import i18n from '../i18n'
import { fmtTokens } from '../lib/format'

// The label maps below keep their Record<string, string> shape (components
// index and Object.entries them) but expose getters, so every read resolves
// through the global composer and stays in sync with locale switches.
/** UI display labels: permission modes / thinking levels / quota windows. */
export const MODE_LABELS: Record<string, string> = {
  get default() {
    return i18n.global.t('agent.modes.default.label')
  },
  get acceptEdits() {
    return i18n.global.t('agent.modes.acceptEdits.label')
  },
  get bypassPermissions() {
    return i18n.global.t('agent.modes.bypassPermissions.label')
  },
  get readOnly() {
    return i18n.global.t('agent.modes.readOnly.label')
  },
  get plan() {
    return i18n.global.t('agent.modes.plan.label')
  },
}

export const MODE_DESCRIPTIONS: Record<string, string> = {
  get default() {
    return i18n.global.t('agent.modes.default.description')
  },
  get acceptEdits() {
    return i18n.global.t('agent.modes.acceptEdits.description')
  },
  get bypassPermissions() {
    return i18n.global.t('agent.modes.bypassPermissions.description')
  },
  get readOnly() {
    return i18n.global.t('agent.modes.readOnly.description')
  },
  get plan() {
    return i18n.global.t('agent.modes.plan.description')
  },
}

export const THINKING_LABELS: Record<string, string> = {
  get off() {
    return i18n.global.t('agent.thinking.levels.off')
  },
  get low() {
    return i18n.global.t('agent.thinking.levels.low')
  },
  get medium() {
    return i18n.global.t('agent.thinking.levels.medium')
  },
  get high() {
    return i18n.global.t('agent.thinking.levels.high')
  },
}

export const THINKING_LEVELS: ThinkingLevel[] = ['off', 'low', 'medium', 'high']

export const THINKING_BUDGETS: Record<string, number> = {
  off: 0,
  low: 4096,
  medium: 10240,
  high: 31999,
}

// Quota window kinds are runtime data ('5h' | 'weekly' | 'mcp'); map them to
// message keys, keeping the raw kind as the display fallback.
const WINDOW_LABEL_KEYS: Record<string, string> = {
  '5h': 'fiveHour',
  weekly: 'weekly',
  mcp: 'mcp',
}

function windowLabel(kind: string): string {
  const key = WINDOW_LABEL_KEYS[kind]
  return key ? i18n.global.t(`agent.quota.windows.${key}`) : kind
}

function resetSuffix(ms: number): string {
  if (!ms) return ''
  const d = new Date(ms)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return i18n.global.t('agent.quota.resetSuffix', { time: `${hh}:${mm}` })
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

  // Short summary for the quota chip: balance-type quotas show the amount
  // directly; plan-type quotas prefer the remaining 5h window.
  const quotaShort = computed(() => {
    const q = quota.value?.quota
    if (!q?.available) return i18n.global.t('agent.quota.shortNone')
    if (q.type === 'balance' && q.balance) return i18n.global.t('agent.quota.shortBalance', { balance: q.balance })
    const parts: string[] = []
    for (const kind of ['5h', 'weekly']) {
      const w = q.windows?.find((x) => x.kind === kind)
      if (w) {
        parts.push(
          i18n.global.t('agent.quota.shortWindow', {
            kind: kind === '5h' ? '5h' : i18n.global.t('agent.quota.weeklyShort'),
            percent: Math.max(0, 100 - w.used_percent).toFixed(0),
          }),
        )
      }
    }
    return parts.join(' · ') || i18n.global.t('agent.quota.shortFallback')
  })

  // Multi-line text for the /quota command and the quota popover.
  const quotaText = computed(() => {
    const q = quota.value
    if (!q) return i18n.global.t('agent.quota.notLoaded')
    const lines: string[] = []
    if (q.quota.available) {
      if (q.quota.type === 'balance') {
        lines.push(i18n.global.t('agent.quota.balanceLine', { balance: q.quota.balance ?? i18n.global.t('agent.quota.unknown') }))
      } else {
        lines.push(
          q.quota.level
            ? i18n.global.t('agent.quota.planRemainingWithLevel', { level: q.quota.level })
            : i18n.global.t('agent.quota.planRemaining'),
        )
        for (const w of q.quota.windows ?? []) {
          const remaining = Math.max(0, 100 - w.used_percent)
          lines.push(
            i18n.global.t('agent.quota.windowLine', {
              label: windowLabel(w.kind),
              remaining: remaining.toFixed(1),
              used: w.used_percent.toFixed(1),
              reset: resetSuffix(w.next_reset_ms),
            }),
          )
        }
      }
    } else {
      lines.push(i18n.global.t('agent.quota.unavailable', { reason: q.quota.reason ?? i18n.global.t('agent.quota.unknownReason') }))
    }
    const s = q.session
    if (s?.requests) {
      const cost =
        s.cost_usd != null && s.cost_known
          ? i18n.global.t('agent.quota.sessionCostSuffix', { usd: s.cost_usd.toFixed(2) })
          : ''
      lines.push(
        i18n.global.t('agent.quota.sessionUsage', {
          requests: s.requests,
          input: fmtTokens(s.input_tokens ?? 0),
          output: fmtTokens(s.output_tokens ?? 0),
        }) + cost,
      )
    }
    return lines.join('\n')
  })

  async function load() {
    try {
      state.value = await api.getAgent()
    } catch {
      state.value = null // the toolbar falls back to placeholder display
    }
  }

  async function setMode(m: string) {
    if (!MODE_LABELS[m]) throw new Error(i18n.global.t('agent.unknownMode', { mode: m }))
    await api.setPermissionMode(m)
    if (state.value) state.value.permission_mode = m
  }

  async function setThinking(level: string) {
    if (!THINKING_LABELS[level]) throw new Error(i18n.global.t('agent.thinking.unknownLevel', { level }))
    if (level !== 'off' && thinkingLevels.value.length && !thinkingLevels.value.includes(level)) {
      throw new Error(i18n.global.t('agent.thinking.unsupportedLevel', { level }))
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
      quotaError.value = toErrorMessage(e)
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
