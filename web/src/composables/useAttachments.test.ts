import { vi } from 'vitest'
import { beforeEach, describe, expect, it } from 'vitest'
import i18n from '../i18n'
import { MAX_ATTACHMENTS, useAttachments } from './useAttachments'

function imageFile(name = 'shot.png', type = 'image/png') {
  return new File([new Uint8Array([1, 2, 3])], name, { type })
}

describe('useAttachments', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
  })

  it('reads images into data-URL attachments', async () => {
    const a = useAttachments()
    a.addFiles([imageFile()])
    await vi.waitFor(() => expect(a.attachments.value.length).toBeGreaterThan(0))
    expect(a.attachments.value).toHaveLength(1)
    expect(a.attachments.value[0]).toMatchObject({ name: 'shot.png', mimeType: 'image/png' })
    expect(a.attachments.value[0].dataUrl).toMatch(/^data:image\/png;base64,/)
    expect(a.attachmentError.value).toBe('')
  })

  it('rejects non-images with the image-only hint', async () => {
    const a = useAttachments()
    a.addFiles([new File([new Uint8Array([1])], 'notes.txt', { type: 'text/plain' })])
    await new Promise((r) => setTimeout(r, 0))
    expect(a.attachments.value).toHaveLength(0)
    expect(a.attachmentError.value).toContain('notes.txt')
  })

  it('caps a pasted batch including pending readers (queued + pending)', async () => {
    const a = useAttachments()
    const batch = Array.from({ length: MAX_ATTACHMENTS + 3 }, (_, i) => imageFile(`b${i}.png`))
    a.addFiles(batch)
    await vi.waitFor(() => expect(a.attachments.value).toHaveLength(MAX_ATTACHMENTS))
    // The cap holds even though every enqueue is asynchronous.
    expect(a.attachments.value).toHaveLength(MAX_ATTACHMENTS)
    expect(a.attachmentError.value).toContain(String(MAX_ATTACHMENTS))
  })

  it('removeAttachment drops exactly one', async () => {
    const a = useAttachments()
    a.addFiles([imageFile('one.png'), imageFile('two.png')])
    await vi.waitFor(() => expect(a.attachments.value.length).toBeGreaterThan(0))
    const first = a.attachments.value[0].id
    a.removeAttachment(first)
    expect(a.attachments.value).toHaveLength(1)
    expect(a.attachments.value[0].name).toBe('two.png')
  })
})
