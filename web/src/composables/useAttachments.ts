import { ref } from 'vue'
import i18n from '../i18n'
import type { Attachment } from '../types'

// The composer's image-attachment tray: pick, paste, cap, remove. Extracted
// from ChatPanel so the cap + pending-reader accounting is testable alone.
//
// The cap must hold for queued + pending together: the real enqueue happens
// asynchronously in reader.onload, so a synchronous length check alone lets
// a whole pasted batch slip past the limit.
export const MAX_ATTACHMENTS = 6

export function useAttachments() {
  const attachments = ref<Attachment[]>([])
  const attachmentError = ref('')
  let attachSeq = 0
  // Files whose FileReader has been started but not yet landed in attachments.
  let pendingCount = 0

  function addFiles(files: File[] | FileList) {
    attachmentError.value = ''
    for (const f of Array.from(files)) {
      if (!f.type.startsWith('image/')) {
        attachmentError.value = i18n.global.t('chat.imageOnly', { name: f.name })
        continue
      }
      pendingCount++
      if (attachments.value.length + pendingCount > MAX_ATTACHMENTS) {
        pendingCount--
        attachmentError.value = i18n.global.t('chat.maxImages', { count: MAX_ATTACHMENTS })
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
      // A failed read never fires onload; release its reservation so the
      // slot is not leaked.
      reader.onerror = () => {
        pendingCount--
      }
      reader.readAsDataURL(f)
    }
  }

  function removeAttachment(id: string) {
    attachments.value = attachments.value.filter((a) => a.id !== id)
  }

  return { attachments, attachmentError, addFiles, removeAttachment }
}
