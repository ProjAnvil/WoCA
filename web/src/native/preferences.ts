import { useDark, useStorage, type RemovableRef, type StorageLike } from '@vueuse/core'
import { getNativeBridge, type NativeBridge } from './bridge'

export const PREFERENCE_KEYS = [
  'woca-theme',
  'woca.pref.locale',
  'woca.pref.autoCollapseTools',
  'woca.pref.markdownPreview',
  'woca.pref.model',
  'woca.pref.permissionMode',
] as const

export type PreferenceKey = (typeof PREFERENCE_KEYS)[number]

const knownKeys = new Set<string>(PREFERENCE_KEYS)
const nativeCache = new Map<PreferenceKey, string>()
let activeBridge: NativeBridge | null = null
let writeTail: Promise<void> = Promise.resolve()
let writeFailed = false

const nativeStorage: StorageLike = {
  getItem(key) {
    assertKnownKey(key)
    return nativeCache.get(key) ?? null
  },
  setItem(key, value) {
    assertKnownKey(key)
    writeNativePreference(key, value)
  },
  removeItem() {
    throw new Error('native preferences cannot be removed')
  },
}

function assertKnownKey(key: string): asserts key is PreferenceKey {
  if (!knownKeys.has(key)) throw new Error('preference key is not allowed')
}

function preferenceStorage(): StorageLike {
  const bridge = getNativeBridge()
  if (bridge) {
    activeBridge = bridge
    return nativeStorage
  }
  return window.localStorage
}

export function readPreference(key: PreferenceKey): string | null {
  return preferenceStorage().getItem(key)
}

export function usePreference<T>(key: PreferenceKey, defaults: T): RemovableRef<T> {
  return useStorage(key, defaults, preferenceStorage())
}

export function useThemePreference() {
  return useDark({ storageKey: 'woca-theme', storage: preferenceStorage() })
}

export async function hydrateNativePreferences(bridge: NativeBridge): Promise<void> {
  activeBridge = bridge
  const values = await Promise.all(
    PREFERENCE_KEYS.map(async (key) => ({ key, preference: await bridge.Preference(key) })),
  )
  const hydrated = new Map<PreferenceKey, string>()
  for (const { key, preference } of values) {
    if (preference.found) hydrated.set(key, preference.value)
  }
  nativeCache.clear()
  for (const [key, value] of hydrated) nativeCache.set(key, value)
}

export function writeNativePreference(key: PreferenceKey, value: string): void {
  const bridge = activeBridge ?? getNativeBridge()
  if (!bridge) throw new Error('native preference bridge is unavailable')
  activeBridge = bridge
  nativeCache.set(key, value)
  writeTail = writeTail.then(async () => {
    try {
      await bridge.PutPreference({ key, value })
    } catch {
      writeFailed = true
    }
  })
}

export async function drainPreferenceWrites(): Promise<void> {
  await writeTail
  if (writeFailed) {
    writeFailed = false
    throw new Error('unable to persist native preferences')
  }
}
