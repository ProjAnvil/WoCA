<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Bot, Brain, Gauge, Layers, Lightbulb, PanelRight, Paperclip, Send, ShieldCheck, Sparkles, Square, Target, X } from 'lucide-vue-next'
import ChatItemView from './ChatItemView.vue'
import { useSessionStore } from '../stores/sessions'
import { useWorkspaceStore } from '../stores/workspace'
import { useCompletionsStore } from '../stores/completions'
import { useMemoryStore } from '../stores/memory'
import { useAgentStore, MODE_DESCRIPTIONS, MODE_LABELS, THINKING_BUDGETS, THINKING_LABELS } from '../stores/agent'
import { useSettingsStore } from '../stores/settings'
import { useServerSettingsStore } from '../stores/serverSettings'
import { useToastStore } from '../stores/toast'
import { useAttachments } from '../composables/useAttachments'
import { useAutocomplete } from '../composables/useAutocomplete'
import { fmtTokens } from '../lib/format'
import type { Attachment, CompletionEntry, ContextCategory, PermissionMode, ThinkingLevel } from '../types'

const { t } = useI18n()
const store = useSessionStore()
const workspace = useWorkspaceStore()
const completions = useCompletionsStore()
const memory = useMemoryStore()
const agent = useAgentStore()
const toast = useToastStore()



const settingsStore = useSettingsStore()
const catalog = useServerSettingsStore()
const draft = ref('')
const textarea = ref<HTMLTextAreaElement | null>(null)

/* --------------------- attachments + autocomplete (composables) ----------- */

const { attachments, attachmentError, addFiles, removeAttachment } = useAttachments()

function onPickFiles(e: Event) {
  const input = e.target as HTMLInputElement
  if (input.files) addFiles(input.files)
  input.value = ''
}

// Cmd+V (macOS) / Ctrl+V (Windows) pastes a screenshot straight into the
// attachment tray; the textarea's default paste keeps text as-is.
function onPaste(e: ClipboardEvent) {
  const items = e.clipboardData?.items
  if (!items) return
  const images: File[] = []
  for (const it of Array.from(items)) {
    if (it.kind === 'file' && it.type.startsWith('image/')) {
      const f = it.getAsFile()
      if (f) images.push(f)
    }
  }
  if (images.length) {
    e.preventDefault()
    addFiles(images)
  }
}

const completionSource = computed(() => ({ skills: completions.skills, agents: completions.agents }))
const { ac, acItems, updateAc, closeAc, onBlurCloseAc, acceptAc, onKeydown } = useAutocomplete({
  textarea,
  draft,
  completions: completionSource,
  onEnter: send,
})

const fileInput = ref<HTMLInputElement | null>(null)

const busy = computed(() => ['running', 'awaiting', 'queued'].includes(store.activeStatus))
// Private-cloud form with an empty model catalog: the deployment offers
// no models — sending is refused until an operator configures one.
const canSend = computed(
  () =>
    (draft.value.trim().length > 0 || attachments.value.length > 0) &&
    !busy.value &&
    !catalog.noModelsManaged,
)

onMounted(() => {
  if (!preferredId.value && catalog.models.length) {
    preferredId.value = catalog.models[0].id
  }
  void completions.load()
  void agent.load()
  void agent.refreshQuota()
  void catalog.ensureModels()
})

/* ------------------------- input toolbar (mode/thinking/quota) ------------ */

type CtlPop = '' | 'mode' | 'thinking' | 'quota' | 'model'
const pop = ref<CtlPop>('')

function togglePop(which: CtlPop) {
  pop.value = pop.value === which ? '' : which
}

/* ----------------------- preferred model (catalog pick) ------------------ */

// The preferred model: picked from the console-configured catalog only
// (the bridge is never hard-bound to one provider — switching verifies
// with the catalog authority and applies from the next model call).
const preferredId = ref(localStorage.getItem('woca.pref.modelPreset') ?? '')

const currentModelName = computed(
  () =>
    catalog.models.find((m) => m.id === preferredId.value)?.name ||
    catalog.models[0]?.name ||
    agent.state?.model ||
    '',
)

async function pickModel(id: string) {
  preferredId.value = id
  localStorage.setItem('woca.pref.modelPreset', id)
  pop.value = ''
  await catalog.applyModel(id)
  await agent.load()
}

async function pickMode(mode: PermissionMode) {
  try {
    await agent.setMode(mode)
  } catch (e) {
    toast.error(e)
    return
  }
  pop.value = ''
}

async function pickThinking(level: ThinkingLevel) {
  try {
    await agent.setThinking(level)
  } catch (e) {
    toast.error(e)
    return
  }
  pop.value = ''
}

// Refresh the quota once before expanding the chip's popover.
async function openQuota() {
  if (pop.value === 'quota') {
    pop.value = ''
    return
  }
  pop.value = 'quota'
  void agent.refreshQuota()
}

/* --------------------------- context indicator (hover) -------------------- */

// The context chip is always visible; hovering opens the breakdown and fires
// the on-demand /context request (VS Code Copilot's pattern: persistent bar +
// hover for the categorized detail). An empty session (nothing sent yet) has
// no usage to show — the chip stays disabled until the first message lands.
const ctxOpen = ref(false)
let ctxLeaveTimer: number | undefined

const ctxAvailable = computed(() => store.activeItems.length > 0)

function ctxEnter() {
  if (!ctxAvailable.value) return
  window.clearTimeout(ctxLeaveTimer)
  ctxOpen.value = true
  if (!store.contextLoading) void store.refreshContext()
}
function ctxLeave() {
  window.clearTimeout(ctxLeaveTimer)
  ctxLeaveTimer = window.setTimeout(() => (ctxOpen.value = false), 150)
}

// The provider-reported total of the most recent model call (input + cache
// reads + cache writes) — the exact counterpart of the estimate.
const ctxExact = computed(() => {
  const lr = store.contextInfo?.last_request
  return lr ? lr.input_tokens + lr.cache_read_tokens + lr.cache_write_tokens : 0
})

const ctxShort = computed(() => {
  if (!store.contextInfo) return store.contextLoading ? '…' : '—'
  return ctxExact.value ? fmtTokens(ctxExact.value) : `~${fmtTokens(store.contextInfo.estimated_total)}`
})

const ctxWindowPct = computed(() => {
  const info = store.contextInfo
  return info?.context_window ? Math.min(100, (info.estimated_total / info.context_window) * 100) : 0
})

const ctxCompactPct = computed(() => {
  const info = store.contextInfo
  return info?.context_window && info.compact_threshold
    ? Math.min(100, (info.compact_threshold / info.context_window) * 100)
    : 0
})

function ctxCatPct(c: ContextCategory): string {
  const total = store.contextInfo?.estimated_total ?? 0
  return total ? `${Math.max(2, (c.tokens / total) * 100).toFixed(1)}%` : '0%'
}

const modeList = Object.entries(MODE_LABELS) as [PermissionMode, string][] as [PermissionMode, string][]


/* ------------------------------------- send ------------------------------- */

function send() {
  const text = draft.value.trim()
  if (!text && attachments.value.length === 0) return
  draft.value = ''
  const atts = attachments.value
  attachments.value = []
  closeAc()
  void store.send(text, atts)
}
</script>

<template>
  <div class="chat">
    <header class="chat-header">
      <div class="chat-title-wrap">
        <span class="chat-title">{{ store.activeSession?.title ?? 'woca' }}</span>
        <span v-if="store.activeStatus === 'queued'" class="run-badge">
          {{ t('chat.queued', { position: store.queuePosition[store.activeId] ?? 1 }) }}
        </span>
        <span v-else-if="store.activeStatus === 'running'" class="run-badge">{{ t('chat.running') }}</span>
        <span v-else-if="store.activeStatus === 'awaiting'" class="run-badge">{{ t('chat.awaitingConfirm') }}</span>
        <button
          v-if="store.activeGoal"
          class="goal-chip"
          :class="`s-${store.activeGoal.status}`"
          :title="t('chat.goalChipTitle')"
          @click="store.goalPanelOpen = true"
        >
          <Target :size="13" />
          <span class="goal-chip-text">{{ store.activeGoal.objective }}</span>
          <span class="goal-chip-meta">C{{ store.activeGoal.cycles }}/{{ store.activeGoal.max_cycles }}</span>
        </button>
      </div>
      <button
        class="icon-btn mem-toggle"
        :title="t('chat.memoryTitle')"
        @click="memory.open(store.activeWorkspace?.path ?? '', store.activeWorkspace?.name)"
      >
        <Brain :size="15" />
        <span class="mem-toggle-label">{{ t('chat.memory') }}</span>
      </button>
      <button
        class="icon-btn panel-toggle"
        :class="{ active: workspace.panelOpen }"
        :title="workspace.panelOpen ? t('chat.collapseWorkspace') : t('chat.expandWorkspace')"
        @click="workspace.togglePanel()"
      >
        <PanelRight :size="17" />
      </button>
    </header>

    <div class="chat-messages">
      <ChatItemView v-for="item in store.activeItems" :key="item.id" :item="item" />
      <div v-if="store.activeStatus === 'error'" class="error-banner">
        {{ store.activeError }}
      </div>
      <div v-if="store.activeItems.length === 0 && !busy" class="empty-hint">
        {{ t('chat.emptyHint') }}
      </div>
    </div>

    <div class="chat-input">
      <!-- Clicking outside the popover closes it -->
      <div v-if="pop" class="ctl-backdrop" @click="pop = ''"></div>

      <!-- Chat-input toolbar: permission mode / thinking level / remaining quota -->
      <div class="input-toolbar">
        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'mode' }"
            :title="t('chat.switchModeTitle')"
            @click="togglePop('mode')"
          >
            <ShieldCheck :size="13" />
            <span>{{ agent.modeLabel }}</span>
          </button>
          <div v-if="pop === 'mode'" class="ctl-pop">
            <div class="ctl-pop-header">{{ t('chat.permissionMode') }}</div>
            <button
              v-for="[m, label] in modeList"
              :key="m"
              class="ctl-opt"
              :class="{ active: agent.mode === m }"
              type="button"
              @click="pickMode(m)"
            >
              <span class="ctl-opt-name">{{ label }}<span class="ctl-opt-key">{{ m }}</span></span>
              <span class="ctl-opt-desc">{{ MODE_DESCRIPTIONS[m] }}</span>
            </button>
          </div>
        </div>

        <div v-if="catalog.models.length" class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'model' }"
            :title="t('chat.modelChange')"
            @click="togglePop('model')"
          >
            <Bot :size="13" />
            <span>{{ currentModelName }}</span>
          </button>
          <div v-if="pop === 'model'" class="ctl-pop">
            <div class="ctl-pop-header">{{ t('chat.modelPick') }}</div>
            <button
              v-for="m in catalog.models"
              :key="m.id"
              class="ctl-opt"
              :class="{ active: (preferredId || catalog.models[0]?.id) === m.id }"
              type="button"
              @click="pickModel(m.id)"
            >
              <span class="ctl-opt-name">{{ m.name }}<span class="ctl-opt-key">{{ m.model }}</span></span>
              <span class="ctl-opt-desc">{{ m.provider }}</span>
            </button>
          </div>
        </div>

        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'thinking', disabled: !agent.thinkingSupported }"
            :title="
              agent.thinkingSupported
                ? t('chat.thinkingChange')
                : t('chat.thinkingUnsupported', { model: agent.state?.model ?? t('chat.currentModel') })
            "
            :disabled="!agent.thinkingSupported"
            @click="agent.thinkingSupported && togglePop('thinking')"
          >
            <Lightbulb :size="13" />
            <span>{{ t('chat.thinkingLabel') }}·{{ agent.thinkingSupported ? agent.thinkingLabel : t('chat.unsupported') }}</span>
          </button>
          <div v-if="pop === 'thinking'" class="ctl-pop">
            <div class="ctl-pop-header">
              {{ t('chat.thinkingLevel') }}
              <span v-if="agent.thinkingNote" class="ctl-pop-note">{{ agent.thinkingNote }}</span>
            </div>
            <button
              v-for="level in (['off', ...agent.thinkingLevels] as ThinkingLevel[])"
              :key="level"
              class="ctl-opt"
              :class="{ active: agent.thinkingLevel === level }"
              type="button"
              @click="pickThinking(level)"
            >
              <span class="ctl-opt-name"
                >{{ THINKING_LABELS[level]
                }}<span class="ctl-opt-key">{{
                  THINKING_BUDGETS[level] ? `${THINKING_BUDGETS[level]} tokens` : t('chat.off')
                }}</span></span
              >
            </button>
          </div>
        </div>

        <div
          class="ctl-wrap"
          @mouseenter="ctxEnter"
          @mouseleave="ctxLeave"
        >
          <button
            class="ctl-chip"
            :class="{ open: ctxOpen, disabled: !ctxAvailable }"
            type="button"
            :title="ctxAvailable ? t('chat.ctxUsageTitle') : t('chat.ctxUsageUnavailable')"
          >
            <Layers :size="13" />
            <span>{{ t('chat.context') }} {{ ctxAvailable ? ctxShort : '—' }}</span>
          </button>
          <div v-if="ctxOpen" class="ctl-pop ctx-pop" @mouseenter="ctxEnter" @mouseleave="ctxLeave">
            <div class="ctl-pop-header">
              {{ t('chat.ctxBreakdown') }}
              <span v-if="store.contextInfo?.model" class="ctl-pop-note">{{ store.contextInfo.model }}</span>
            </div>
            <div class="ctx-body">
              <div v-if="store.contextError" class="ctx-error">{{ store.contextError }}</div>
              <template v-else-if="store.contextInfo">
                <div v-if="ctxExact" class="ctx-exact">
                  <span class="ctx-exact-num">{{ fmtTokens(ctxExact) }}</span>
                  <span class="ctx-exact-detail">
                    {{
                      t('chat.ctxExactDetail', {
                        input: fmtTokens(store.contextInfo.last_request!.input_tokens),
                        cacheRead: fmtTokens(store.contextInfo.last_request!.cache_read_tokens),
                        cacheWrite: fmtTokens(store.contextInfo.last_request!.cache_write_tokens),
                      })
                    }}
                  </span>
                </div>
                <div v-else class="ctx-exact-detail">{{ t('chat.ctxNoExact') }}</div>
                <div class="ctx-estimate">
                  {{ t('chat.ctxEstimate') }} <b>~{{ fmtTokens(store.contextInfo.estimated_total) }}</b> tokens
                </div>
                <div v-if="store.contextInfo.context_window" class="ctx-window">
                  <div class="ctx-bar">
                    <div class="ctx-bar-fill" :style="{ width: ctxWindowPct + '%' }"></div>
                    <div
                      v-if="ctxCompactPct"
                      class="ctx-bar-mark"
                      :style="{ left: ctxCompactPct + '%' }"
                      :title="t('chat.compactThreshold')"
                    ></div>
                  </div>
                  <div class="ctx-bar-label">
                    {{
                      t('chat.ctxWindowLabel', {
                        used: fmtTokens(store.contextInfo.estimated_total),
                        total: fmtTokens(store.contextInfo.context_window),
                        pct: ctxWindowPct.toFixed(0),
                      })
                    }}
                  </div>
                </div>
                <div v-if="store.contextInfo.compact_threshold" class="ctx-compact">
                  {{ t('chat.compactThreshold') }} {{ fmtTokens(store.contextInfo.compact_threshold) }} tokens
                </div>
                <div class="ctx-cats">
                  <div v-for="c in store.contextInfo.categories" :key="c.key" class="ctx-cat">
                    <div class="ctx-cat-head">
                      <span class="ctx-cat-label">
                        {{ c.label }}<span v-if="c.detail" class="ctx-cat-detail">{{ c.detail }}</span>
                      </span>
                      <span class="ctx-cat-tokens">
                        ~{{ fmtTokens(c.tokens) }}<span v-if="c.count"> · {{ c.count }}</span>
                      </span>
                    </div>
                    <div class="ctx-cat-bar"><div class="ctx-cat-fill" :style="{ width: ctxCatPct(c) }"></div></div>
                  </div>
                </div>
                <div class="ctx-foot">{{ t('chat.ctxFoot') }}</div>
              </template>
              <div v-else class="ctx-foot">{{ t('common.loading') }}</div>
            </div>
          </div>
        </div>

        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'quota' }"
            :title="agent.quotaShort === t('agent.quota.shortNone') ? t('chat.viewQuota') : agent.quotaShort"
            @click="openQuota"
          >
            <Gauge :size="13" />
            <span>{{ agent.quotaLoading ? t('chat.querying') : agent.quotaShort }}</span>
          </button>
          <div v-if="pop === 'quota'" class="ctl-pop quota-pop">
            <div class="ctl-pop-header">
              {{ t('chat.quota') }}
              <button class="ctl-refresh" type="button" @click="agent.refreshQuota()">{{ t('common.refresh') }}</button>
            </div>
            <div class="quota-body">{{ agent.quotaError || agent.quotaText }}</div>
          </div>
        </div>
      </div>

      <!-- autocomplete popup: / for skills, @ for subagents -->
      <div v-if="ac.open && acItems.length" class="ac-popup">
        <div class="ac-header">
          {{ ac.kind === 'skill' ? t('chat.acSkills') : t('chat.acAgents') }}
        </div>
        <div class="ac-list">
          <button
            v-for="(item, i) in acItems"
            :key="item.name"
            class="ac-item"
            :class="{ active: i === ac.index }"
            type="button"
            @mousedown.prevent="acceptAc(item)"
            @mousemove="ac.index = i"
          >
            <Sparkles v-if="ac.kind === 'skill'" :size="14" class="ac-icon" />
            <Bot v-else :size="14" class="ac-icon" />
            <span class="ac-name">{{ ac.kind === 'skill' ? '/' : '@' }}{{ item.name }}</span>
            <span class="ac-desc" :title="item.description">{{ item.description }}</span>
          </button>
        </div>
        <div v-if="acItems[ac.index]?.description" class="ac-detail">
          {{ acItems[ac.index].description }}
        </div>
      </div>

      <div v-if="attachments.length || attachmentError" class="attach-tray">
        <div v-for="a in attachments" :key="a.id" class="attach-chip">
          <img :src="a.dataUrl" :alt="a.name" class="attach-thumb" />
          <button class="attach-remove" :title="t('chat.remove')" @click="removeAttachment(a.id)">
            <X :size="10" />
          </button>
          <span class="attach-name">{{ a.name }}</span>
        </div>
        <span v-if="attachmentError" class="attach-error">{{ attachmentError }}</span>
      </div>

      <div class="chat-input-inner">
        <button
          class="icon-btn attach-btn"
          :title="t('chat.uploadImage')"
          @click="fileInput?.click()"
        >
          <Paperclip :size="16" />
        </button>
        <input
          ref="fileInput"
          type="file"
          accept="image/*"
          multiple
          hidden
          @change="onPickFiles"
        />
        <textarea
          ref="textarea"
          v-model="draft"
          rows="1"
          :placeholder="t('chat.placeholder')"
          @input="updateAc"
          @keydown="onKeydown"
          @paste="onPaste"
          @blur="onBlurCloseAc"
        ></textarea>
        <button v-if="!busy" class="send-btn" :disabled="!canSend" @click="send">
          <Send :size="16" />
        </button>
        <button v-else class="send-btn stop" :title="t('chat.stop')" @click="store.cancel()">
          <Square :size="14" />
        </button>
      </div>
    </div>
  </div>
</template>
