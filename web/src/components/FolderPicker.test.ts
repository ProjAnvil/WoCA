// The folder browser: load/drill/pick, mkdir with server confirm, manual
// path escape hatch, and error/empty states.
import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({ fsList: vi.fn(), fsMkdir: vi.fn() }))
vi.mock('../api/client', () => ({ ...api }))

import i18n from '../i18n'
import FolderPicker from './FolderPicker.vue'

const listing = (over: Partial<Awaited<ReturnType<typeof api.fsList>>>) =>
  ({
    path: '/home/u',
    parent: '/home',
    dirs: [],
    roots: ['/home', '/data'],
    ...over,
  }) as never

const mountPicker = () => mount(FolderPicker, { global: { plugins: [i18n, createPinia()] } })
const settled = async (w: ReturnType<typeof mountPicker>) => {
  await Promise.resolve()
  await Promise.resolve()
  await w.vm.$nextTick()
}

beforeEach(() => {
  vi.clearAllMocks()
  api.fsList.mockReset().mockResolvedValue(
    listing({ dirs: [{ name: 'proj', path: '/home/u/proj', mtime: 0, writable: true }] }),
  )
})

describe('folder picker', () => {
  it('loads the root listing on mount and drills into a directory', async () => {
    const w = mountPicker()
    await settled(w)
    expect(api.fsList).toHaveBeenCalledWith(undefined)
    expect(w.findAll('.fp-row')).toHaveLength(1)
    // Breadcrumb shows the deep path (not one of the roots as current).
    expect(w.get('.fp-crumb-current').text()).toBe('/home/u')

    await w.get('.fp-row').trigger('click')
    expect(api.fsList).toHaveBeenLastCalledWith('/home/u/proj')
  })

  it('single-click navigates; the check button picks (emits select)', async () => {
    const w = mountPicker()
    await settled(w)
    await w.get('.fp-pick').trigger('click')
    expect(w.emitted('select')?.[0]).toEqual(['/home/u/proj'])
  })

  it('navigates up via the parent button and refreshes via the toolbar', async () => {
    const w = mountPicker()
    await settled(w)
    await w.get('.fp-actions .icon-btn').trigger('click') // up
    expect(api.fsList).toHaveBeenLastCalledWith('/home')
    await w.findAll('.fp-actions .icon-btn')[1].trigger('click') // refresh
    expect(api.fsList).toHaveBeenLastCalledWith('/home/u')
  })

  it('shows the empty state without dirs and the error channel on failures', async () => {
    api.fsList.mockResolvedValueOnce(listing({ dirs: [] }))
    const w = mountPicker()
    await settled(w)
    expect(w.get('.fp-empty').text()).toBe(i18n.global.t('fs.empty'))

    api.fsList.mockRejectedValueOnce(new Error('bridge fsb off'))
    const w2 = mountPicker()
    await settled(w2)
    expect(w2.get('.fp-error').text()).toContain('bridge fsb off')
  })

  it('mkdir confirms server-side, reloads in place and clears the input', async () => {
    api.fsMkdir.mockResolvedValueOnce({ path: '/home/u/new' })
    const w = mountPicker()
    await settled(w)
    const input = w.get('.fp-mkdir input')
    expect(w.get('.fp-mkdir .btn').attributes('disabled')).toBeDefined() // empty name
    await input.setValue('new')
    expect(w.get('.fp-mkdir .btn').attributes('disabled')).toBeUndefined()
    await input.trigger('keydown.enter')
    expect(api.fsMkdir).toHaveBeenCalledWith('/home/u', 'new')
    await settled(w)
    expect((input.element as HTMLInputElement).value).toBe('')
    expect(api.fsList).toHaveBeenLastCalledWith('/home/u')

    api.fsMkdir.mockRejectedValueOnce(new Error('exists'))
    await input.setValue('dupe')
    await w.get('.fp-mkdir .btn').trigger('click')
    await settled(w)
    expect(w.get('.fp-error').text()).toContain('exists')
  })

  it('manual path entry emits select and the backdrop closes', async () => {
    const w = mountPicker()
    await settled(w)
    const input = w.get('.fp-manual input')
    expect(w.get('.fp-manual .btn').attributes('disabled')).toBeDefined()
    await input.setValue('  /srv/manual  ')
    await w.get('.fp-manual .btn').trigger('click')
    expect(w.emitted('select')?.[0]).toEqual(['/srv/manual'])

    await w.get('.modal-backdrop').trigger('click')
    expect(w.emitted('close')).toBeTruthy()
  })
})
