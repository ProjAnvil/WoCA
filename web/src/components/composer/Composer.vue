<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Bot, Paperclip, Send, Sparkles, Square } from 'lucide-vue-next'
import QueueBadge from './QueueBadge.vue'
import SteeringBadge from './SteeringBadge.vue'
import AttachmentTray from './AttachmentTray.vue'
import ComposerChips from './ComposerChips.vue'
import { useSessionStore } from '../../stores/sessions'
import { useCompletionsStore } from '../../stores/completions'
import { useAgentStore, PERMISSION_MODE_ORDER } from '../../stores/agent'
import { useServerSettingsStore } from '../../stores/serverSettings'
import { useToastStore } from '../../stores/toast'
import { useAttachments } from '../../composables/useAttachments'
import { useAutocomplete } from '../../composables/useAutocomplete'
import { useMessageQueue } from '../../composables/useMessageQueue'
import { useWorkspaceStore } from '../../stores/workspace'
import { useBtwStore } from '../../stores/btw'
import type { PermissionMode } from '../../types'

// The composer: queue badge + chip toolbar + autocomplete + attachment
// tray + the text row. Sending during a run parks the message instead of
// locking the input; the touch entry for that is the send button, which
// stays available (as a secondary "queue send") alongside the stop button.
const { t } = useI18n()
const store = useSessionStore()
const completions = useCompletionsStore()
const agent = useAgentStore()
const catalog = useServerSettingsStore()
const toast = useToastStore()
const workspaceStore = useWorkspaceStore()
const btw = useBtwStore()

const draft = ref('')
const textarea = ref<HTMLTextAreaElement | null>(null)

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

const fileInput = ref<HTMLInputElement | null>(null)

const busy = computed(() => ['running', 'awaiting', 'queued'].includes(store.activeStatus))
// Private-cloud form with an empty model catalog: the deployment offers
// no models — sending is refused until an operator configures one.
// Busy no longer blocks sending: a send during a run parks as the next
// message (useMessageQueue) instead of locking the composer.
const canSend = computed(
  () => (draft.value.trim().length > 0 || attachments.value.length > 0) && !catalog.noModelsManaged,
)

onMounted(() => {
  void completions.load()
})

/* ------------------------------- autocomplete ------------------------------ */

const completionSource = computed(() => ({ skills: completions.skills, agents: completions.agents }))
const { ac, acItems, updateAc, closeAc, onBlurCloseAc, acceptAc, onKeydown } = useAutocomplete({
  textarea,
  draft,
  completions: completionSource,
  onEnter: send,
})

/* ---------------------------- mode cycling (Shift+Tab) --------------------- */

// Shift+Tab cycles the permission mode right in the composer (Cursor /
// Claude Code convention). The autocomplete popup owns plain Tab (accept),
// and IME composition must never trigger a mode switch.
function cycleMode(e: KeyboardEvent) {
  if (ac.value.open || e.isComposing) return
  e.preventDefault()
  const cur = PERMISSION_MODE_ORDER.indexOf(agent.mode as PermissionMode)
  const next =
    PERMISSION_MODE_ORDER[(cur + 1 + PERMISSION_MODE_ORDER.length) % PERMISSION_MODE_ORDER.length] ??
    PERMISSION_MODE_ORDER[0]
  void agent.setMode(next).catch((err) => toast.error(err))
}

/* ---------------------------------- send ----------------------------------- */

// Next-message queue: sending mid-run parks the message; the moment the
// run idles it flushes automatically (edit/cancel available on the badge).
const { queued, enqueue, cancel: cancelQueued, takeForEdit } = useMessageQueue(busy, (text, atts) =>
  store.send(text, atts),
)

function editQueued() {
  const q = takeForEdit()
  if (!q) return
  draft.value = q.text
  attachments.value = q.attachments
  textarea.value?.focus()
}

function send() {
  const text = draft.value.trim()
  if (!text && attachments.value.length === 0) return
  // "/btw " routes to the side channel: answered, never persisted to the
  // transcript (the panel stays open for follow-ups).
  if (text.startsWith('/btw ') && !store.demo) {
    draft.value = ''
    closeAc()
    void btw.ask(text.slice(5), store.activeId, workspaceStore.root || undefined)
    return
  }
  draft.value = ''
  const atts = attachments.value
  attachments.value = []
  const comments = workspaceStore.takeComments()
  closeAc()
  if (busy.value) {
    // A run actually in flight can be steered immediately (Codex parity);
    // attachments/inline comments still need the queue-and-flush path since
    // steering only carries plain text.
    if (store.activeStatus === 'running' && atts.length === 0 && comments.length === 0) {
      store.steer(text)
    } else {
      enqueue(text, atts, comments)
    }
    return
  }
  void store.send(text, atts, comments)
}

/** Fills the draft (empty-state sample prompts) and focuses the input. */
function fillPrompt(text: string) {
  draft.value = text
  textarea.value?.focus()
}

defineExpose({ fillPrompt })
</script>

<template>
  <div class="chat-input">
    <SteeringBadge
      v-if="store.activePendingSteers.length"
      :count="store.activePendingSteers.length"
      :text="store.activePendingSteers[store.activePendingSteers.length - 1]"
    />
    <QueueBadge v-if="queued" :text="queued.text" @edit="editQueued" @cancel="cancelQueued" />

    <ComposerChips />

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

    <AttachmentTray :attachments="attachments" :error="attachmentError" @remove="removeAttachment" />

    <div class="chat-input-inner">
      <button
        class="icon-btn attach-btn"
        :title="t('chat.uploadImage')"
        :aria-label="t('chat.uploadImage')"
        @click="fileInput?.click()"
      >
        <Paperclip :size="16" />
      </button>
      <input ref="fileInput" type="file" accept="image/*" multiple hidden @change="onPickFiles" />
      <textarea
        ref="textarea"
        v-model="draft"
        rows="1"
        :placeholder="t('chat.placeholder')"
        @input="updateAc"
        @keydown="onKeydown"
        @keydown.shift.tab="cycleMode"
        @paste="onPaste"
        @blur="onBlurCloseAc"
      ></textarea>
      <Transition name="fade">
        <button
          v-if="busy"
          class="send-btn stop"
          :title="store.activePendingSteers.length ? t('chat.stopAndResend') : t('chat.stop')"
          :aria-label="store.activePendingSteers.length ? t('chat.stopAndResend') : t('chat.stop')"
          @click="store.cancel()"
        >
          <Square :size="14" />
        </button>
      </Transition>
      <button
        class="send-btn"
        :class="{ 'queue-send': busy }"
        :disabled="!canSend"
        :title="busy ? (store.activeStatus === 'running' ? t('chat.sendSteer') : t('chat.sendQueued')) : t('chat.send')"
        :aria-label="busy ? (store.activeStatus === 'running' ? t('chat.sendSteer') : t('chat.sendQueued')) : t('chat.send')"
        @click="send"
      >
        <Send :size="16" />
      </button>
    </div>
  </div>
</template>
