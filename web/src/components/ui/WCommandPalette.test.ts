import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import i18n from '../../i18n'
import WCommandPalette from './WCommandPalette.vue'
import { usePaletteStore } from '../../stores/palette'

const commands = [
  { id: 'panel:settings', group: 'panels' as const, label: 'Open settings', run: vi.fn() },
  { id: 'session:1', group: 'sessions' as const, label: 'Refactor the parser', hint: 'woca', run: vi.fn() },
  { id: 'mode:plan', group: 'mode' as const, label: 'Switch to Plan', keywords: 'plan', run: vi.fn() },
]

function mountPalette() {
  // One pinia instance shared by the app (plugin) and the test (active) —
  // two instances would give the component its own invisible store.
  const pinia = createPinia()
  setActivePinia(pinia)
  return mount(WCommandPalette, {
    props: { commands },
    global: { plugins: [pinia, i18n] },
    attachTo: document.body,
  })
}

describe('WCommandPalette', () => {
  it('renders only while open and filters by query', async () => {
    const w = mountPalette()
    const palette = usePaletteStore()
    expect(document.querySelector('.w-palette')).toBeNull()

    palette.toggle()
    await flushPromises()
    const input = document.querySelector<HTMLInputElement>('.w-palette-input')!
    expect(input).toBeTruthy()

    expect(document.querySelectorAll('.w-palette-item').length).toBe(commands.length)

    input.value = 'settings'
    input.dispatchEvent(new Event('input'))
    await flushPromises()
    const labels = Array.from(document.querySelectorAll('.w-palette-item-label')).map((el) => el.textContent)
    expect(labels).toEqual(['Open settings'])

    w.unmount()
  })

  it('Enter runs the selected command and closes', async () => {
    const w = mountPalette()
    const palette = usePaletteStore()
    palette.toggle()
    await flushPromises()

    const input = document.querySelector<HTMLInputElement>('.w-palette-input')!
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }))
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }))
    await flushPromises()

    expect(commands[1].run).toHaveBeenCalled() // first ↓ moves to index 1
    expect(palette.open).toBe(false)
    w.unmount()
  })

  it('shows the empty state for unmatched queries', async () => {
    const w = mountPalette()
    const palette = usePaletteStore()
    palette.toggle()
    await flushPromises()

    const input = document.querySelector<HTMLInputElement>('.w-palette-input')!
    input.value = 'zzzz'
    input.dispatchEvent(new Event('input'))
    await flushPromises()
    expect(document.querySelector('.w-palette-empty')).toBeTruthy()

    w.unmount()
  })
})
