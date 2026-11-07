// The output tail block: renders the polled tail in mono with the
// truncation note, restarts the watcher on retry after a failed read, and
// stops polling on unmount (collapse path).
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../../i18n'
import OutputTail from './OutputTail.vue'
import { useActivityStore } from '../../stores/activity'
import type { OutputTail as OutputTailData } from '../../types'

const api = vi.hoisted(() => ({
  getActivity: vi.fn(),
  getShellOutput: vi.fn(),
  getTaskOutput: vi.fn(),
  killShell: vi.fn(),
  stopTask: vi.fn(),
  deleteCron: vi.fn(),
}))
vi.mock('../../api/client', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  ...api,
}))

const tail = (over: Partial<OutputTailData> = {}): OutputTailData => ({
  id: 'sh1',
  status: 'running',
  output: 'hello tail',
  truncated: false,
  total_bytes: 11,
  ...over,
})

beforeEach(() => {
  vi.useFakeTimers()
  setActivePinia(createPinia())
  vi.resetAllMocks()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('OutputTail', () => {
  it('starts the watcher on mount and renders the polled output', async () => {
    api.getShellOutput.mockResolvedValue(tail())
    const w = mount(OutputTail, { props: { sid: 's1', kind: 'shell', id: 'sh1' }, global: { plugins: [i18n] } })
    for (let i = 0; i < 10; i++) await Promise.resolve()
    expect(api.getShellOutput).toHaveBeenCalledWith('s1', 'sh1', 65536)
    expect(w.get('.act-output-text').text()).toContain('hello tail')
    w.unmount()
  })

  it('shows the truncation note with shown/total sizes', async () => {
    api.getTaskOutput.mockResolvedValue(tail({ truncated: true, output: 'x'.repeat(2048), total_bytes: 9000 }))
    const w = mount(OutputTail, { props: { sid: 's1', kind: 'task', id: 't1' }, global: { plugins: [i18n] } })
    for (let i = 0; i < 10; i++) await Promise.resolve()
    expect(api.getTaskOutput).toHaveBeenCalledWith('s1', 't1', 65536)
    expect(w.get('.act-output-note').text()).toContain('2 KB')
    expect(w.get('.act-output-note').text()).toContain('9 KB')
    w.unmount()
  })

  it('a failed read shows the inline error; retry starts a fresh watcher', async () => {
    api.getShellOutput.mockRejectedValueOnce(new Error('tail read failed'))
    const w = mount(OutputTail, { props: { sid: 's1', kind: 'shell', id: 'sh1' }, global: { plugins: [i18n] } })
    for (let i = 0; i < 10; i++) await Promise.resolve()
    expect(w.get('.act-output-error').text()).toContain('tail read failed')
    expect(useActivityStore().outputErrorOf('s1', 'shell', 'sh1')).toBe('tail read failed')

    api.getShellOutput.mockResolvedValue(tail({ output: 'recovered' }))
    await w.get('.act-output-error .act-link-btn').trigger('click')
    for (let i = 0; i < 10; i++) await Promise.resolve()
    expect(api.getShellOutput).toHaveBeenCalledTimes(2)
    expect(w.get('.act-output-text').text()).toContain('recovered')
    w.unmount()
  })

  it('stops polling when unmounted (row collapsed)', async () => {
    api.getShellOutput.mockResolvedValue(tail())
    const w = mount(OutputTail, { props: { sid: 's1', kind: 'shell', id: 'sh1' }, global: { plugins: [i18n] } })
    for (let i = 0; i < 10; i++) await Promise.resolve()
    w.unmount()
    const calls = api.getShellOutput.mock.calls.length
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.getShellOutput).toHaveBeenCalledTimes(calls)
  })
})
