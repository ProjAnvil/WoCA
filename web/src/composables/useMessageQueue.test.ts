import { describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import { useMessageQueue } from './useMessageQueue'

describe('useMessageQueue', () => {
  it('parks a message and flushes it when the run goes idle', async () => {
    const busy = ref(true)
    const send = vi.fn()
    const { queued, enqueue } = useMessageQueue(busy, send)

    enqueue('next up', [])
    expect(queued.value).toMatchObject({ text: 'next up' })

    busy.value = false
    await vi.waitFor(() => expect(send).toHaveBeenCalledWith('next up', [], undefined))
    expect(queued.value).toBeNull()
  })

  it('flushes only the latest parked message (last write wins)', async () => {
    const busy = ref(true)
    const send = vi.fn()
    const { enqueue } = useMessageQueue(busy, send)

    enqueue('first', [])
    enqueue('second', [])
    busy.value = false
    await vi.waitFor(() => expect(send).toHaveBeenCalledTimes(1))
    expect(send).toHaveBeenCalledWith('second', [], undefined)
  })

  it('takeForEdit hands the message back and empties the slot', () => {
    const busy = ref(true)
    const { queued, enqueue, takeForEdit } = useMessageQueue(busy, vi.fn())
    enqueue('parked', [])
    const q = takeForEdit()
    expect(q).toMatchObject({ text: 'parked' })
    expect(queued.value).toBeNull()
  })

  it('cancel drops the message without flushing', async () => {
    const busy = ref(true)
    const send = vi.fn()
    const { enqueue, cancel } = useMessageQueue(busy, send)
    enqueue('gone', [])
    cancel()
    busy.value = false
    expect(send).not.toHaveBeenCalled()
  })

  it('does not flush when nothing is parked', () => {
    const send = vi.fn()
    const busy = ref(true)
    useMessageQueue(busy, send)
    busy.value = false
    expect(send).not.toHaveBeenCalled()
  })
})
