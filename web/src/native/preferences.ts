import {
  customStorageEventName,
  useDark,
  useStorage,
  type RemovableRef,
  type StorageLike,
} from '@vueuse/core'
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

interface PreferenceState {
  confirmed: string | null
  confirmedRevision: number
  desired: string | null
  desiredRevision: number
  retryValue: string | null
}

const knownKeys = new Set<string>(PREFERENCE_KEYS)
const nativeState = new Map<PreferenceKey, PreferenceState>()
const pendingWrites = new Set<Promise<void>>()
const revisionKey = Symbol.for('woca.native.preferenceRevision')
const revisionHost = globalThis as unknown as Record<PropertyKey, unknown>
let activeBridge: NativeBridge | null = null

if (typeof revisionHost[revisionKey] !== 'number') {
  const origin = typeof performance === 'undefined' ? Date.now() : performance.timeOrigin
  revisionHost[revisionKey] = Math.floor(origin * 1000)
}

const nativeStorage: StorageLike = {
  getItem(key) {
    assertKnownKey(key)
    return stateFor(key).desired
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
  const storage = preferenceStorage()
  if (storage === nativeStorage) {
    // Missing SQLite values stay absent until the user changes them. This also
    // lets a failed first write reconcile to the typed default without VueUse
    // immediately turning that reconciliation into another persistence call.
    return useStorage(key, defaults, storage, { writeDefaults: false })
  }
  return useStorage(key, defaults, storage)
}

export function useThemePreference() {
  const storage = preferenceStorage()
  if (storage === nativeStorage) {
    // useColorMode does not forward all UseStorageOptions, so provide the
    // storage ref explicitly to keep a missing native value absent.
    const storageRef = useStorage<'auto' | 'dark' | 'light'>('woca-theme', 'auto', storage, {
      writeDefaults: false,
    })
    return useDark({ storageRef })
  }
  return useDark({ storageKey: 'woca-theme', storage })
}

export async function hydrateNativePreferences(bridge: NativeBridge): Promise<void> {
  activeBridge = bridge
  const values = await Promise.all(
    PREFERENCE_KEYS.map(async (key) => ({ key, preference: await bridge.Preference(key) })),
  )
  const hydrated = new Map<PreferenceKey, PreferenceState>()
  for (const { key, preference } of values) {
    const value = preference.found ? preference.value : null
    hydrated.set(key, {
      confirmed: value,
      confirmedRevision: 0,
      desired: value,
      desiredRevision: 0,
      retryValue: null,
    })
  }
  nativeState.clear()
  for (const [key, state] of hydrated) nativeState.set(key, state)
}

export function writeNativePreference(key: PreferenceKey, value: string): void {
  setDesiredAndSend(key, value, false)
}

export function retryFailedNativePreference(key: PreferenceKey): boolean {
  const state = stateFor(key)
  if (state.retryValue === null) return false
  setDesiredAndSend(key, state.retryValue, true)
  return true
}

function setDesiredAndSend(key: PreferenceKey, value: string, notifyDesired: boolean): void {
  const bridge = activeBridge ?? getNativeBridge()
  if (!bridge) throw new Error('native preference bridge is unavailable')
  activeBridge = bridge

  const state = stateFor(key)
  const oldValue = state.desired
  const revision = nextRevision()
  state.desired = value
  state.desiredRevision = revision
  state.retryValue = null
  if (notifyDesired) notifyStorageChange(key, oldValue, value)

  let request: Promise<PreferenceWriteResult>
  try {
    request = bridge.SetPreference({ key, value, revision })
  } catch {
    request = Promise.reject(new Error('native preference write failed'))
  }
  const operation = request
    .then((result) => completeWrite(key, value, revision, result))
    .catch(() => failWrite(key, value, revision))
  pendingWrites.add(operation)
  void operation.finally(() => pendingWrites.delete(operation))
}

export async function drainPreferenceWrites(): Promise<void> {
  while (pendingWrites.size > 0) await Promise.all([...pendingWrites])
  if ([...nativeState.values()].some((state) => state.retryValue !== null)) {
    throw new Error('unable to persist native preferences')
  }
}

function completeWrite(
  key: PreferenceKey,
  value: string,
  revision: number,
  result: PreferenceWriteResult,
): void {
  if (result.revision !== revision || !result.applied) {
    failWrite(key, value, revision)
    return
  }
  const state = stateFor(key)
  if (revision < state.confirmedRevision) return
  const previousConfirmed = state.confirmed
  state.confirmed = value
  state.confirmedRevision = revision
  if (state.retryValue !== null && state.desired === previousConfirmed) {
    const oldValue = state.desired
    state.desired = value
    if (state.retryValue === value) state.retryValue = null
    notifyStorageChange(key, oldValue, value)
  }
  if (state.desiredRevision === revision) state.retryValue = null
}

function failWrite(key: PreferenceKey, value: string, revision: number): void {
  const state = stateFor(key)
  if (state.desiredRevision !== revision || state.desired !== value) return
  const oldValue = state.desired
  state.desired = state.confirmed
  state.retryValue = value
  notifyStorageChange(key, oldValue, state.confirmed)
}

function stateFor(key: PreferenceKey): PreferenceState {
  let state = nativeState.get(key)
  if (!state) {
    state = {
      confirmed: null,
      confirmedRevision: 0,
      desired: null,
      desiredRevision: 0,
      retryValue: null,
    }
    nativeState.set(key, state)
  }
  return state
}

function nextRevision(): number {
  const current = revisionHost[revisionKey]
  if (typeof current !== 'number' || !Number.isSafeInteger(current) || current >= Number.MAX_SAFE_INTEGER) {
    throw new Error('native preference revision is unavailable')
  }
  const revision = current + 1
  revisionHost[revisionKey] = revision
  return revision
}

function notifyStorageChange(key: PreferenceKey, oldValue: string | null, newValue: string | null): void {
  window.dispatchEvent(
    new CustomEvent(customStorageEventName, {
      detail: { key, oldValue, newValue, storageArea: nativeStorage },
    }),
  )
}
