import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const getStatus = vi.fn()
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, getStatus: (...a: Parameters<typeof getStatus>) => getStatus(...a) }
})

import { useStatusStore } from './status'

describe('useStatusStore', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    getStatus.mockReset()
  })

  it('lamp: grey standalone, green synced, yellow STALE, red on failure', async () => {
    const s = useStatusStore()
    expect(s.lamp).toBe('err') // nothing probed yet

    getStatus.mockResolvedValue({ edition: 'personal', managed: false })
    await s.refresh()
    expect(s.lamp).toBe('off') // enrolled=false → standalone

    getStatus.mockResolvedValue({
      edition: 'bridge',
      managed: true,
      cloud: { enrolled: true, stale: false } as never,
    })
    await s.refresh()
    expect(s.lamp).toBe('ok')

    getStatus.mockResolvedValue({
      edition: 'bridge',
      managed: true,
      cloud: { enrolled: true, stale: true } as never,
    })
    await s.refresh()
    expect(s.lamp).toBe('stale')

    getStatus.mockRejectedValue(new Error('unreachable'))
    await s.refresh()
    expect(s.lamp).toBe('err')
    // The last good snapshot stays readable for the modal's zones.
    expect(s.status?.cloud?.stale).toBe(true)
  })
})
