import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const api = vi.hoisted(() => ({
  getClaudeEnv: vi.fn(),
  putClaudeEnv: vi.fn(),
  getPlugins: vi.fn(),
  putPlugins: vi.fn(),
  installPlugin: vi.fn(),
  getManagedDelivery: vi.fn(),
}))
vi.mock('../api/client', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api/client')>()
  return { ...orig, ...api }
})

import { useClaudeEnvStore, useManagedStore, usePluginsStore } from './sections'
import type { PluginStatus } from '../types'

function plugin(over: Partial<PluginStatus> = {}): PluginStatus {
  return {
    name: 'browser',
    kind: 'golang',
    version: '0.1.0',
    description: '',
    tools: [],
    runtime: 'pinchtab',
    enabled: false,
    state: 'not_installed',
    ...over,
  } as PluginStatus
}

describe('settings section stores', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const fn of Object.values(api)) fn.mockReset()
  })

  describe('usePluginsStore', () => {
    it('loads the inventory and keeps a failed toggle from flipping the switch', async () => {
      const browser = plugin()
      const pdf = plugin({ name: 'pdf', enabled: true })
      api.getPlugins.mockResolvedValue({ plugins: [browser, pdf] })
      api.putPlugins.mockRejectedValue(new Error('forbidden'))

      const s = usePluginsStore()
      await s.load()
      expect(s.state.plugins.map((p) => p.name)).toEqual(['browser', 'pdf'])

      await s.toggle(browser)
      expect(api.putPlugins).toHaveBeenCalledWith(['browser', 'pdf'])
      expect(browser.enabled).toBe(false) // failed toggle leaves state untouched
      expect(s.state.error).toBe('forbidden')
    })

    it('a successful toggle sends the enabled set and reports the message', async () => {
      const browser = plugin({ state: 'ready' })
      api.getPlugins.mockResolvedValue({ plugins: [browser] })
      api.putPlugins.mockResolvedValue({})

      const s = usePluginsStore()
      await s.load()
      await s.toggle(browser)
      expect(api.putPlugins).toHaveBeenCalledWith(['browser'])
      expect(browser.enabled).toBe(true)
      expect(s.state.message).toContain('browser')
    })

    it('install marks the plugin installing and arms the progress poller', async () => {
      vi.useFakeTimers()
      const browser = plugin()
      api.getPlugins
        .mockResolvedValueOnce({ plugins: [browser] })
        .mockResolvedValue({ plugins: [plugin({ state: 'ready', enabled: true })] })
      api.installPlugin.mockResolvedValue({})
      api.putPlugins.mockResolvedValue({})

      const s = usePluginsStore()
      await s.load()
      await s.install(browser, false)
      expect(s.state.plugins[0].state).toBe('installing')
      expect(s.state.message).toContain('browser')

      // Poller fires while installing, then disarms once the run settles.
      await vi.advanceTimersByTimeAsync(2100)
      expect(api.getPlugins).toHaveBeenCalledTimes(2) // initial load + one poll
      await vi.advanceTimersByTimeAsync(4000)
      expect(api.getPlugins).toHaveBeenCalledTimes(2) // disarmed: nothing installing

      s.reset()
      await vi.advanceTimersByTimeAsync(4000)
      expect(api.getPlugins).toHaveBeenCalledTimes(2) // reset cleared the timer
      vi.useRealTimers()
    })

    it('reset drops the inventory and flags a reload', async () => {
      api.getPlugins.mockResolvedValue({ plugins: [plugin()] })
      const s = usePluginsStore()
      await s.load()
      expect(s.state.loaded).toBe(true)
      s.reset()
      expect(s.state.loaded).toBe(false)
      expect(s.state.plugins).toEqual([])
    })
  })

  describe('useClaudeEnvStore', () => {
    it('toggles selections locally and PUTs them on save', async () => {
      api.getClaudeEnv.mockResolvedValue({
        skills: [{ name: 'deploy', description: '' }],
        mcp: [],
        selected: { skills: [], mcp: [] },
      })
      api.putClaudeEnv.mockResolvedValue({})
      api.getClaudeEnv.mockResolvedValueOnce({
        skills: [{ name: 'deploy', description: '' }],
        mcp: [],
        selected: { skills: [], mcp: [] },
      })

      const s = useClaudeEnvStore()
      await s.load()
      s.toggleSkill('deploy')
      await s.save()
      expect(api.putClaudeEnv).toHaveBeenCalledWith({ skills: ['deploy'], mcp: [] })
      expect(s.state.message).toBeTruthy()
    })

    it('a failed load surfaces the error without pretending success', async () => {
      api.getClaudeEnv.mockRejectedValue(new Error('forbidden'))
      const s = useClaudeEnvStore()
      await s.load()
      expect(s.state.error).toBe('forbidden')
      expect(s.state.loaded).toBe(false)
    })
  })

  describe('useManagedStore', () => {
    it('exposes the delivered inventory', async () => {
      api.getManagedDelivery.mockResolvedValue({
        skills: [{ name: 'deploy' }],
        mcp: [],
      })
      const s = useManagedStore()
      await s.load()
      expect(s.state.delivery?.skills[0].name).toBe('deploy')
      expect(s.state.loaded).toBe(true)
    })
  })
})
