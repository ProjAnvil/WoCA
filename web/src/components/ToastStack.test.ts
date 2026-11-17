// Toast renderer: explicit labeled close button (the whole-toast click
// target was keyboard-unreachable and unannounced), action button wiring,
// and the audit contract that the body itself no longer dismisses.
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ToastStack from './ToastStack.vue'
import { useToastStore } from '../stores/toast'

// One pinia per test, shared by the component and the assertions (mounting
// with a fresh createPinia() would install a second, different store).
let toast: ReturnType<typeof useToastStore>

function mountStack() {
  const pinia = createPinia()
  setActivePinia(pinia)
  const w = mount(ToastStack, { global: { plugins: [i18n, pinia] } })
  toast = useToastStore()
  return w
}

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('ToastStack', () => {
  it('renders an explicit, labeled close button per toast', async () => {
    const w = mountStack()
    toast.push('saved', 'success')
    await w.vm.$nextTick()
    const close = w.get('.toast-close')
    expect(close.element.tagName).toBe('BUTTON')
    expect(close.attributes('aria-label')).toBe(i18n.global.t('common.close'))
  })

  it('the close button dismisses its toast', async () => {
    const w = mountStack()
    toast.push('saved', 'success')
    await w.vm.$nextTick()
    await w.get('.toast-close').trigger('click')
    expect(toast.toasts).toHaveLength(0)
    expect(w.find('.toast').exists()).toBe(false)
  })

  it('the toast body is no longer a click-to-dismiss target (audit change)', async () => {
    const w = mountStack()
    toast.push('stay put', 'info')
    await w.vm.$nextTick()
    await w.get('.toast').trigger('click')
    expect(toast.toasts).toHaveLength(1)
  })

  it('the action button runs its handler and dismisses', async () => {
    const run = vi.fn()
    const w = mountStack()
    toast.push('conflict', 'warn', 60_000, { label: 'Review', run })
    await w.vm.$nextTick()
    const action = w.get('.toast-action')
    expect(action.text()).toBe('Review')
    await action.trigger('click')
    expect(run).toHaveBeenCalledTimes(1)
    expect(toast.toasts).toHaveLength(0)
  })
})
