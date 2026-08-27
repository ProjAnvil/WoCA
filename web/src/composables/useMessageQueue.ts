import { ref, watch, type Ref } from 'vue'
import type { Attachment } from '../types'

export interface QueuedMessage {
  text: string
  attachments: Attachment[]
  comments?: Array<{ file: string; line: number; text: string }>
}

/**
 * Next-message queueing (the Amp pattern): while a run is busy, sending
 * parks the message instead of locking the composer. The moment the run
 * goes idle the parked message flushes automatically; a flush failure is
 * the caller's (send)'s error surface — the text is already out of the
 * queue, so the badge never lies about what will happen next.
 *
 * One slot, last-write-wins: sending again while something is queued
 * replaces it (the edit affordance covers deliberate changes).
 */
export function useMessageQueue(
  busy: Ref<boolean>,
  send: (text: string, attachments: Attachment[], comments?: QueuedMessage['comments']) => unknown,
) {
  const queued = ref<QueuedMessage | null>(null)

  function enqueue(text: string, attachments: Attachment[], comments?: QueuedMessage['comments']) {
    queued.value = { text, attachments, comments }
  }

  function cancel() {
    queued.value = null
  }

  /** Returns the parked message for restoring into the composer, or null. */
  function takeForEdit(): QueuedMessage | null {
    const q = queued.value
    queued.value = null
    return q
  }

  watch(busy, (isBusy, wasBusy) => {
    if (wasBusy && !isBusy && queued.value) {
      const q = queued.value
      queued.value = null
      void send(q.text, q.attachments, q.comments)
    }
  })

  return { queued, enqueue, cancel, takeForEdit }
}
