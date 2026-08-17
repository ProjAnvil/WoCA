<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Bot, Brain, Gauge, Lightbulb, PanelRight, Paperclip, Send, ShieldCheck, Sparkles, Square, Target, X } from 'lucide-vue-next'
import ChatItemView from './ChatItemView.vue'
import { useSessionStore } from '../stores/sessions'
import { useWorkspaceStore } from '../stores/workspace'
import { useCompletionsStore } from '../stores/completions'
import { useMemoryStore } from '../stores/memory'
import { useAgentStore, MODE_DESCRIPTIONS, MODE_LABELS, THINKING_BUDGETS, THINKING_LABELS } from '../stores/agent'
import type { Attachment, CompletionEntry } from '../types'

const store = useSessionStore()
const workspace = useWorkspaceStore()
const completions = useCompletionsStore()
const memory = useMemoryStore()
const agent = useAgentStore()
const draft = ref('')
const textarea = ref<HTMLTextAreaElement | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)

const busy = computed(() => ['running', 'awaiting', 'queued'].includes(store.activeStatus))
const canSend = computed(
  () => (draft.value.trim().length > 0 || attachments.value.length > 0) && !busy.value,
)

onMounted(() => {
  void completions.load()
  void agent.load()
  void agent.refreshQuota()
})

/* ------------------------- input toolbar (mode/thinking/quota) ------------ */

type CtlPop = '' | 'mode' | 'thinking' | 'quota'
const pop = ref<CtlPop>('')

function togglePop(which: CtlPop) {
  pop.value = pop.value === which ? '' : which
}

async function pickMode(mode: string) {
  try {
    await agent.setMode(mode)
  } catch (e) {
    alert(e instanceof Error ? e.message : String(e))
    return
  }
  pop.value = ''
}

async function pickThinking(level: string) {
  try {
    await agent.setThinking(level)
  } catch (e) {
    alert(e instanceof Error ? e.message : String(e))
    return
  }
  pop.value = ''
}

// 点额度 chip 时先刷新一次再展开。
async function openQuota() {
  if (pop.value === 'quota') {
    pop.value = ''
    return
  }
  pop.value = 'quota'
  void agent.refreshQuota()
}

const modeList = Object.entries(MODE_LABELS)

/* ---------------------------------- attachments --------------------------- */

const attachments = ref<Attachment[]>([])
const attachmentError = ref('')
const MAX_ATTACHMENTS = 6
let attachSeq = 0
// Files whose FileReader has been started but not yet landed in attachments.
// The cap must hold for queued + pending together: the real enqueue happens
// asynchronously in reader.onload, so a synchronous length check alone lets a
// whole pasted batch slip past the limit.
let pendingCount = 0

function addFiles(files: File[] | FileList) {
  attachmentError.value = ''
  for (const f of Array.from(files)) {
    if (!f.type.startsWith('image/')) {
      attachmentError.value = `仅支持图片附件：${f.name}`
      continue
    }
    pendingCount++
    if (attachments.value.length + pendingCount > MAX_ATTACHMENTS) {
      pendingCount--
      attachmentError.value = `最多 ${MAX_ATTACHMENTS} 张图片`
      break
    }
    const reader = new FileReader()
    reader.onload = () => {
      pendingCount--
      const dataUrl = String(reader.result ?? '')
      const base64 = dataUrl.slice(dataUrl.indexOf(',') + 1)
      attachments.value = [
        ...attachments.value,
        {
          id: `att-${Date.now().toString(36)}-${attachSeq++}`,
          name: f.name || 'screenshot.png',
          mimeType: f.type,
          dataUrl,
          base64,
        },
      ]
    }
    // A failed read never fires onload; release its reservation so the slot
    // is not leaked.
    reader.onerror = () => {
      pendingCount--
    }
    reader.readAsDataURL(f)
  }
}

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

function removeAttachment(id: string) {
  attachments.value = attachments.value.filter((a) => a.id !== id)
}

/* --------------------------------- autocomplete --------------------------- */

// "/" at the start of the line lists skills; "@" anywhere lists subagents.
// ArrowUp/Down move the highlight, Enter/Tab accept, Esc dismisses.
type AcKind = 'skill' | 'agent'
const ac = ref<{ open: boolean; kind: AcKind; query: string; start: number; index: number }>({
  open: false,
  kind: 'skill',
  query: '',
  start: 0,
  index: 0,
})

const acItems = computed<CompletionEntry[]>(() => {
  if (!ac.value.open) return []
  const q = ac.value.query.toLowerCase()
  const all = ac.value.kind === 'skill' ? completions.skills : completions.agents
  const scored = all.filter((e) => {
    const n = e.name.toLowerCase()
    return n.startsWith(q) || n.includes(q)
  })
  return scored.slice(0, 12)
})

function updateAc() {
  const el = textarea.value
  if (!el) return
  const before = draft.value.slice(0, el.selectionStart ?? draft.value.length)

  const skill = before.match(/^\/([a-zA-Z0-9_-]*)$/)
  if (skill) {
    setAc('skill', skill[1], 0)
    return
  }
  const agent = before.match(/(^|\s)@([a-zA-Z0-9_-]*)$/)
  if (agent) {
    // index of the '@' trigger inside the draft
    setAc('agent', agent[2], before.length - agent[2].length - 1)
    return
  }
  ac.value.open = false
}

// start is the draft index of the trigger character ('/' or '@'); the query
// to filter by begins at start+1.
function setAc(kind: AcKind, query: string, start: number) {
  const changed = query !== ac.value.query || kind !== ac.value.kind || !ac.value.open
  ac.value = { open: true, kind, query, start, index: changed ? 0 : ac.value.index }
}

function closeAc() {
  ac.value.open = false
}

// Delay so clicking a popup item fires before the popup unmounts.
function onBlurCloseAc() {
  window.setTimeout(closeAc, 150)
}

function acceptAc(entry?: CompletionEntry) {
  const target = entry ?? acItems.value[ac.value.index]
  if (!target || !textarea.value) return
  const el = textarea.value
  const caret = el.selectionStart ?? draft.value.length
  const prefix = ac.value.kind === 'skill' ? '/' : '@'
  draft.value =
    draft.value.slice(0, ac.value.start) + prefix + target.name + ' ' + draft.value.slice(caret)
  closeAc()
  el.focus()
}

function onKeydown(e: KeyboardEvent) {
  if (ac.value.open && acItems.value.length) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      ac.value.index = (ac.value.index + 1) % acItems.value.length
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      ac.value.index = (ac.value.index - 1 + acItems.value.length) % acItems.value.length
      return
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault()
      acceptAc()
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      closeAc()
      return
    }
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    send()
  }
}

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
          排队中 #{{ store.queuePosition[store.activeId] ?? 1 }}…
        </span>
        <span v-else-if="store.activeStatus === 'running'" class="run-badge">运行中…</span>
        <span v-else-if="store.activeStatus === 'awaiting'" class="run-badge">等待你的确认…</span>
        <button
          v-if="store.activeGoal"
          class="goal-chip"
          :class="`s-${store.activeGoal.status}`"
          title="目标模式：点击查看详情"
          @click="store.goalPanelOpen = true"
        >
          <Target :size="13" />
          <span class="goal-chip-text">{{ store.activeGoal.objective }}</span>
          <span class="goal-chip-meta">C{{ store.activeGoal.cycles }}/{{ store.activeGoal.max_cycles }}</span>
        </button>
      </div>
      <button
        class="icon-btn mem-toggle"
        title="查看这个工作区的记忆"
        @click="memory.open(store.activeWorkspace?.path ?? '', store.activeWorkspace?.name)"
      >
        <Brain :size="15" />
        <span class="mem-toggle-label">记忆</span>
      </button>
      <button
        class="icon-btn panel-toggle"
        :class="{ active: workspace.panelOpen }"
        :title="workspace.panelOpen ? '收起工作区文件' : '展开工作区文件'"
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
        开始一个新对话吧。
      </div>
    </div>

    <div class="chat-input">
      <!-- 点击弹层外的区域关闭 -->
      <div v-if="pop" class="ctl-backdrop" @click="pop = ''"></div>

      <!-- 聊天框工具条：权限模式 / 思考级别 / 剩余额度 -->
      <div class="input-toolbar">
        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'mode' }"
            title="切换权限模式"
            @click="togglePop('mode')"
          >
            <ShieldCheck :size="13" />
            <span>{{ agent.modeLabel }}</span>
          </button>
          <div v-if="pop === 'mode'" class="ctl-pop">
            <div class="ctl-pop-header">权限模式</div>
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

        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'thinking', disabled: !agent.thinkingSupported }"
            :title="
              agent.thinkingSupported
                ? '修改思考级别'
                : `${agent.state?.model ?? '当前模型'} 不支持思考，选择器已关闭`
            "
            :disabled="!agent.thinkingSupported"
            @click="agent.thinkingSupported && togglePop('thinking')"
          >
            <Lightbulb :size="13" />
            <span>思考·{{ agent.thinkingSupported ? agent.thinkingLabel : '不支持' }}</span>
          </button>
          <div v-if="pop === 'thinking'" class="ctl-pop">
            <div class="ctl-pop-header">
              思考级别（下一轮生效）
              <span v-if="agent.thinkingNote" class="ctl-pop-note">{{ agent.thinkingNote }}</span>
            </div>
            <button
              v-for="level in ['off', ...agent.thinkingLevels]"
              :key="level"
              class="ctl-opt"
              :class="{ active: agent.thinkingLevel === level }"
              type="button"
              @click="pickThinking(level)"
            >
              <span class="ctl-opt-name"
                >{{ THINKING_LABELS[level]
                }}<span class="ctl-opt-key">{{
                  THINKING_BUDGETS[level] ? `${THINKING_BUDGETS[level]} tokens` : '关闭'
                }}</span></span
              >
            </button>
          </div>
        </div>

        <div class="ctl-wrap">
          <button
            class="ctl-chip"
            :class="{ open: pop === 'quota' }"
            :title="agent.quotaShort === '额度 —' ? '查看剩余额度' : agent.quotaShort"
            @click="openQuota"
          >
            <Gauge :size="13" />
            <span>{{ agent.quotaLoading ? '查询中…' : agent.quotaShort }}</span>
          </button>
          <div v-if="pop === 'quota'" class="ctl-pop quota-pop">
            <div class="ctl-pop-header">
              剩余额度
              <button class="ctl-refresh" type="button" @click="agent.refreshQuota()">刷新</button>
            </div>
            <div class="quota-body">{{ agent.quotaError || agent.quotaText }}</div>
          </div>
        </div>
      </div>

      <!-- autocomplete popup: / for skills, @ for subagents -->
      <div v-if="ac.open && acItems.length" class="ac-popup">
        <div class="ac-header">
          {{ ac.kind === 'skill' ? '命令 / Skills' : '子代理 / Subagents' }}
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
          <button class="attach-remove" title="移除" @click="removeAttachment(a.id)">
            <X :size="10" />
          </button>
          <span class="attach-name">{{ a.name }}</span>
        </div>
        <span v-if="attachmentError" class="attach-error">{{ attachmentError }}</span>
      </div>

      <div class="chat-input-inner">
        <button
          class="icon-btn attach-btn"
          title="上传图片附件"
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
          placeholder="给 woca 发送消息…（/ 命令，@ 子代理，可粘贴截图）"
          @input="updateAc"
          @keydown="onKeydown"
          @paste="onPaste"
          @blur="onBlurCloseAc"
        ></textarea>
        <button v-if="!busy" class="send-btn" :disabled="!canSend" @click="send">
          <Send :size="16" />
        </button>
        <button v-else class="send-btn stop" title="停止" @click="store.cancel()">
          <Square :size="14" />
        </button>
      </div>
    </div>
  </div>
</template>
