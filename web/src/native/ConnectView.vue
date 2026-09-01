<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  CloudUpload,
  Globe,
  HardDrive,
  KeyRound,
  LoaderCircle,
  MonitorSmartphone,
  ShieldCheck,
  Trash2,
  TriangleAlert,
} from 'lucide-vue-next'
import {
  getNativeBridge,
  type EmbeddedCloudStatus,
  type EnrollEmbeddedInput,
  type EnrollResult,
  type ProfileSummary,
} from './bridge'
import { validateEnterpriseOrigin } from './origin'

const props = defineProps<{
  serverUrl?: string
  authorizationRequired?: boolean
  /** Entry ① (wails only): the embedded bridge's base URL. */
  embeddedUrl?: string
  connect: (input: ConnectionInput) => Promise<unknown>
  /** One-tap resume of a saved profile along its stored session. */
  resume?: (id: number) => Promise<unknown>
  /** Embedded-bridge enrollment (the three-field registration view). */
  enroll?: (input: EnrollEmbeddedInput) => Promise<EnrollResult>
  /** Deregister the embedded bridge (back to local mode). */
  deregister?: () => Promise<EnrollResult>
}>()

const emit = defineEmits<{ 'connect-embedded': [] }>()

const { t } = useI18n()
// The form area is a small step machine: the remote-connection form, the
// edge bridge picker, and — separated per product decision — the embedded
// bridge's registration ('enroll') and registered sign-in ('enroll-login')
// views. Registration is a machine-plane action owned by this machine, not
// a fourth flavor of "connect to a remote server", so it never appears as
// a tab next to them.
type Tab = 'private' | 'direct'
type Step = 'form' | 'bridges' | 'enroll' | 'enroll-login'
const tab = ref<Tab>('private')
const step = ref<Step>('form')
const serverUrl = ref(props.serverUrl ?? '')
const apiKey = ref('')
const validation = ref('')
const error = ref('')
const pending = ref(false)
// Bridge selection step (entry ②): the edge fronts several bridges.
const bridges = ref<EdgeBridge[]>([])

// Saved connections (the one-tap resume list): profiles with a live
// session continue directly; expired ones prefill the form below. The
// listing is optional surface on older binaries — absence is not an error.
const native = getNativeBridge()
const profiles = ref<ProfileSummary[]>([])
const pendingProfileId = ref<number | null>(null)
const deletingProfileId = ref<number | null>(null)
const confirmingDeleteId = ref<number | null>(null)
const profileNote = ref('')
const apiKeyInput = ref<HTMLInputElement | null>(null)

// The embedded bridge's enrollment state (optional surface — absent on
// dev builds and older binaries). Drives the embedded card's shape: the
// zero-input local entry, the registration link, or the registered badge
// with its sign-in/deregister actions.
const cloud = ref<EmbeddedCloudStatus | null>(null)

// Enrollment form (three fields: edge URL + one-time token + API key).
const enrollUrl = ref('')
const enrollToken = ref('')
const enrollKey = ref('')
const enrollValidation = ref('')
const enrollError = ref('')
const enrollPending = ref(false)

// Registered sign-in (single field) + deregister's two-step confirm.
const loginKey = ref('')
const loginError = ref('')
const loginPending = ref(false)
const confirmingDeregister = ref(false)
const deregisterPending = ref(false)
const deregisterNote = ref('')

onMounted(async () => {
  if (!native?.Profiles) return
  try {
    profiles.value = (await native.Profiles()) ?? []
  } catch {
    // The form below is the fallback path; a failed listing must not
    // block connecting.
  }
})

// The enrollment probe rides the same mount as the profile listing (both
// optional surfaces; absence is not an error).
onMounted(async () => {
  if (!native?.EmbeddedCloudStatus || !props.embeddedUrl) return
  try {
    cloud.value = (await native.EmbeddedCloudStatus()) ?? null
  } catch {
    // Unavailable → the unenrolled card shape; the flow surfaces errors.
  }
})

function profileLabel(p: ProfileSummary): string {
  try {
    return new URL(p.server_url).host
  } catch {
    return p.server_url
  }
}

function profileBadgeKey(p: ProfileSummary): string {
  if (p.session_state === 'active') return 'native.profiles.signedIn'
  if (p.session_state === 'expired') return 'native.profiles.expired'
  return 'native.profiles.signedOut'
}

async function useProfile(p: ProfileSummary) {
  if (pendingProfileId.value !== null || deletingProfileId.value !== null) return
  if (p.session_state === 'active') {
    if (!props.resume) return
    pendingProfileId.value = p.id
    error.value = ''
    try {
      await props.resume(p.id)
    } catch {
      error.value = t('native.connectionError')
    } finally {
      pendingProfileId.value = null
    }
    return
  }
  // Expired or signed out: prefill the form below and focus the key.
  serverUrl.value = p.server_url
  validation.value = ''
  error.value = ''
  await nextTick()
  apiKeyInput.value?.focus()
}

function armDelete(p: ProfileSummary) {
  confirmingDeleteId.value = p.id
  profileNote.value = ''
}

function cancelDelete() {
  confirmingDeleteId.value = null
}

async function doDelete(p: ProfileSummary) {
  if (deletingProfileId.value !== null) return
  deletingProfileId.value = p.id
  profileNote.value = ''
  try {
    const out = await native?.DeleteProfile?.(p.id)
    profiles.value = profiles.value.filter((x) => x.id !== p.id)
    confirmingDeleteId.value = null
    // A failed server-side revocation never blocks the local deletion —
    // report the residual session so the user can chase it, no fallback
    // promises.
    if (out?.had_session && !out.revoked) {
      profileNote.value = t('native.profiles.revokeFailed', { label: profileLabel(p) })
    }
  } catch {
    profileNote.value = t('native.connectionError')
  } finally {
    deletingProfileId.value = null
  }
}

const urlLabel = computed(() => (tab.value === 'private' ? t('native.urlLabelPrivate') : t('native.urlLabelDirect')))
const urlPlaceholder = computed(() =>
  tab.value === 'private' ? t('native.urlPlaceholderPrivate') : t('native.urlPlaceholderDirect'),
)

watch(
  () => props.serverUrl,
  (value) => {
    serverUrl.value = value ?? ''
  },
)
watch(tab, () => {
  validation.value = ''
  error.value = ''
})
watch(step, () => {
  // Step transitions clear only their own step's errors — a class-token
  // enrollment failure (e.g. a rejected key routing to sign-in) carries
  // its message across the move.
  if (step.value !== 'enroll') enrollValidation.value = ''
  if (step.value !== 'enroll-login') loginError.value = ''
})

async function useEmbedded() {
  error.value = ''
  pending.value = true
  try {
    emit('connect-embedded')
  } finally {
    pending.value = false
  }
}

// —— Embedded-bridge registration (machine plane + user login in one
// guided flow). Failures are class tokens; all terminal — the flow never
// downgrades to "enroll anyway".

function openEnroll() {
  step.value = 'enroll'
  enrollError.value = ''
  enrollValidation.value = ''
}

function openEnrollLogin() {
  step.value = 'enroll-login'
  loginError.value = ''
}

async function submitEnroll() {
  if (enrollPending.value) return
  enrollValidation.value = ''
  enrollError.value = ''
  const url = enrollUrl.value.trim()
  const token = enrollToken.value.trim()
  const key = enrollKey.value.trim()
  if (!url || !token || !key) {
    enrollValidation.value = t('native.enroll.invalidInput')
    return
  }
  if (!token.startsWith('woca_enroll_')) {
    enrollValidation.value = t('native.enroll.invalidToken')
    return
  }
  const origin = validateEnterpriseOrigin(url)
  if (!origin) {
    enrollValidation.value = t('native.invalidOrigin')
    return
  }
  if (!props.enroll) {
    enrollError.value = t('native.enroll.errUnavailable')
    return
  }
  enrollPending.value = true
  try {
    const res = await props.enroll({ edge_url: origin, enrollment_token: token, api_key: key })
    if (!res.error) {
      // Ready state applied upstream; the view unmounts.
      enrollToken.value = ''
      enrollKey.value = ''
      return
    }
    if (res.error === 'user_key') {
      // The machine IS enrolled — route to the single-key sign-in and say
      // what failed (the two planes reported separately, no rollback).
      cloud.value = { available: true, enrolled: true, bridge_id: res.bridge_id, edge_url: origin }
      loginError.value = t('native.enroll.errKey')
      loginKey.value = key
      step.value = 'enroll-login'
      enrollToken.value = ''
      enrollKey.value = ''
      return
    }
    const keyMap: Record<string, string> = {
      edge_unreachable: 'native.enroll.errEdge',
      enroll_token: 'native.enroll.errToken',
      bridge_write: 'native.enroll.errWrite',
      bridge_restart: 'native.enroll.errFlip',
    }
    enrollError.value = t(keyMap[res.error] ?? 'native.enroll.errUnavailable')
  } catch {
    enrollError.value = t('native.connectionError')
  } finally {
    enrollPending.value = false
  }
}

async function submitEnrollLogin() {
  if (loginPending.value) return
  loginError.value = ''
  const key = loginKey.value.trim()
  const edge = cloud.value?.edge_url
  const bridgeId = cloud.value?.bridge_id
  if (!key || !edge) {
    loginError.value = t('native.enroll.invalidInput')
    return
  }
  loginPending.value = true
  try {
    await props.connect({ server_url: edge, api_key: key, bridge_id: bridgeId })
    loginKey.value = '' // succeeded — the view unmounts
  } catch {
    // A failed attempt keeps the key in the field (memory only, never
    // persisted) so a retry does not force the user to retype it.
    loginError.value = t('native.connectionError')
  } finally {
    loginPending.value = false
  }
}

async function doDeregister() {
  if (deregisterPending.value) return
  deregisterPending.value = true
  deregisterNote.value = ''
  try {
    const res = await props.deregister?.()
    if (!res) {
      deregisterNote.value = t('native.enroll.errUnavailable')
      return
    }
    if (res.error) {
      const keyMap: Record<string, string> = {
        bridge_write: 'native.enroll.errWrite',
        bridge_restart: 'native.enroll.errFlip',
      }
      deregisterNote.value = t(keyMap[res.error] ?? 'native.enroll.errUnavailable')
      return
    }
    // Success: back to the unenrolled connect page.
    confirmingDeregister.value = false
    cloud.value = { available: true, enrolled: false }
    step.value = 'form'
  } finally {
    deregisterPending.value = false
  }
}

async function submit() {
  validation.value = ''
  error.value = ''
  if (!serverUrl.value.trim() || !apiKey.value) {
    validation.value = t('native.invalidInput')
    return
  }
  // Direct link form (design §6.6 LIST alternative):
  // https://edge.example.com/?bridge=<id> pins the target up front —
  // strip the query before origin validation (which rejects queries).
  let directBridge = ''
  const raw = serverUrl.value.trim()
  const qm = raw.indexOf('?')
  if (qm >= 0) {
    const m = /(?:^|&)(?:bridge|bridge_id)=([^&\s]+)/.exec(raw.slice(qm + 1))
    // Only a bridge-pinning query is stripped and honored; any other
    // query stays and fails origin validation (unknown query = reject).
    if (m) {
      directBridge = decodeURIComponent(m[1])
      serverUrl.value = raw.slice(0, qm)
    }
  }
  const origin = validateEnterpriseOrigin(serverUrl.value)
  if (!origin) {
    validation.value = t('native.invalidOrigin')
    return
  }
  pending.value = true
  keepKey = false
  try {
    serverUrl.value = origin
    if (directBridge) {
      await doConnect(directBridge)
      apiKey.value = '' // succeeded — the view unmounts; clear for symmetry
      return
    }
    // Entry ② semantics: an edge may front several bridges — ask for the
    // pre-auth inventory and let the user pick. Unavailable inventories
    // (older binaries, non-native) fall through to a plain connect; a
    // single bridge needs no choice (the edge auto-routes).
    if (tab.value === 'private') {
      const native = getNativeBridge()
      if (native?.EdgeBridges) {
        try {
          const list = await native.EdgeBridges(origin)
          if (list.length === 0) {
            error.value = t('native.noBridges')
            return
          }
          if (list.length > 1) {
            bridges.value = list
            step.value = 'bridges'
            keepKey = true // the pick needs it; pickBridge clears it
            return
          }
        } catch {
          // Inventory unreachable: let the connection attempt surface
          // the real error.
        }
      }
    }
    await doConnect()
    apiKey.value = '' // succeeded — the view unmounts; clear for symmetry
  } catch {
    // A failed attempt keeps the key in the field (memory only, never
    // persisted) so a retry does not force the user to retype it.
    error.value = t('native.connectionError')
  } finally {
    pending.value = false
  }
}

// keepKey marks the selection-step transition: the API key must survive
// until the user picks the target bridge (pickBridge clears it).
let keepKey = false

async function doConnect(bridgeId?: string) {
  const input: ConnectionInput = { server_url: serverUrl.value, api_key: apiKey.value }
  if (bridgeId) input.bridge_id = bridgeId
  await props.connect(input)
}

async function pickBridge(id: string) {
  pending.value = true
  try {
    await doConnect(id)
    apiKey.value = '' // succeeded — the view unmounts; clear for symmetry
  } catch {
    // Failed pick keeps the key in memory (never persisted) so a retry or
    // another bridge does not force the user to retype it.
    error.value = t('native.connectionError')
  } finally {
    pending.value = false
  }
}

function backToForm() {
  step.value = 'form'
  bridges.value = []
  error.value = ''
}
</script>

<template>
  <main class="native-gate">
    <div class="gate-orb gate-orb--indigo" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--violet" aria-hidden="true"></div>
    <div class="gate-orb gate-orb--cyan" aria-hidden="true"></div>

    <section class="gate-card">
      <div class="gate-brand">
        <div class="gate-logo" aria-hidden="true">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
            <path
              d="M4 7l2.4 10h2.2L11 9.6 13.4 17h2.2L18 7h-2.1l-1.6 7.2L12 7h-2L7.7 14.2 6.1 7H4z"
              fill="currentColor"
            />
          </svg>
        </div>
        <span class="gate-wordmark">WOCA</span>
        <span class="gate-brand-tag">CONNECT</span>
      </div>

      <h1>{{ t('native.title') }}</h1>
      <p class="gate-sub">{{ t('native.subtitle') }}</p>

      <p v-if="authorizationRequired" class="gate-notice">
        <TriangleAlert :size="15" aria-hidden="true" />
        {{ t('native.authorizationRequired') }}
      </p>

      <!-- Entry ①: the embedded bridge (wails only) — auto-launched,
           loopback-trusted. Two shapes: the zero-input local entry (an
           unenrolled bridge), or the registered badge with sign-in and
           deregister (an enrolled bridge runs the apikey posture — local
           zero-login use no longer exists, so the entry says so honestly). -->
      <div v-if="embeddedUrl" data-testid="embedded-entry" class="gate-embedded" :class="{ enrolled: cloud?.enrolled }">
        <div class="gate-embedded-text">
          <template v-if="!cloud?.enrolled">
            <strong>{{ t('native.embeddedTitle') }}</strong>
            <span>{{ t('native.embeddedHint') }}</span>
          </template>
          <template v-else>
            <strong>
              {{ t('native.enroll.registeredBadge') }}
              <code v-if="cloud?.bridge_id" class="gate-embedded-id">{{ cloud.bridge_id }}</code>
            </strong>
            <span>{{ t('native.enroll.loginTitle') }}</span>
          </template>
        </div>
        <div class="gate-embedded-actions">
          <button
            v-if="!cloud?.enrolled"
            type="button"
            data-testid="embedded-connect"
            class="gate-embedded-btn"
            @click="useEmbedded"
          >
            <HardDrive :size="15" aria-hidden="true" />
            {{ t('native.embeddedUse') }}
          </button>
          <button
            v-if="!cloud?.enrolled"
            type="button"
            data-testid="embedded-enroll-open"
            class="gate-embedded-btn gate-embedded-btn--secondary"
            @click="openEnroll"
          >
            <CloudUpload :size="15" aria-hidden="true" />
            {{ t('native.enroll.open') }}
          </button>
          <button
            v-if="cloud?.enrolled"
            type="button"
            data-testid="embedded-signin"
            class="gate-embedded-btn"
            @click="openEnrollLogin"
          >
            <KeyRound :size="15" aria-hidden="true" />
            {{ t('native.enroll.login') }}
          </button>
          <button
            v-if="cloud?.enrolled"
            type="button"
            data-testid="embedded-deregister-arm"
            class="gate-embedded-btn gate-embedded-btn--danger"
            @click="confirmingDeregister = true"
          >
            {{ t('native.enroll.deregister') }}
          </button>
        </div>
      </div>

      <!-- Deregister: inline two-step confirm (the gate page has no app
           dialog store — same pattern as the profile delete confirm). -->
      <div v-if="cloud?.enrolled && confirmingDeregister" data-testid="embedded-deregister-confirm" class="gate-profile-confirm">
        <span class="gate-profile-confirm-text">{{ t('native.enroll.deregisterBody') }}</span>
        <span class="gate-profile-confirm-actions">
          <button type="button" class="gate-ghost" @click="confirmingDeregister = false">
            {{ t('common.cancel') }}
          </button>
          <button
            type="button"
            class="gate-profile-confirm-delete"
            data-testid="embedded-deregister-confirm-btn"
            :disabled="deregisterPending"
            @click="doDeregister"
          >
            <LoaderCircle v-if="deregisterPending" class="gate-spinner" :size="13" aria-hidden="true" />
            {{ t('native.enroll.deregisterConfirm') }}
          </button>
        </span>
      </div>
      <p v-if="deregisterNote" class="gate-profile-note" role="status">{{ deregisterNote }}</p>

      <!-- Saved connections: profiles with a live session resume in one
           tap (the list orders them first); expired ones prefill the
           form. Deleting removes the profile and its local session. -->
      <div v-if="profiles.length" data-testid="saved-profiles" class="gate-saved">
        <p class="gate-step-title">{{ t('native.profiles.title') }}</p>
        <ul class="gate-profiles">
          <li v-for="p in profiles" :key="p.id" class="gate-profile-row">
            <div class="gate-profile">
              <button
                type="button"
                class="gate-profile-main"
                :disabled="pendingProfileId !== null || deletingProfileId !== null"
                @click="useProfile(p)"
              >
                <LoaderCircle
                  v-if="pendingProfileId === p.id"
                  class="gate-spinner"
                  :size="15"
                  aria-hidden="true"
                />
                <span v-else class="gate-profile-dot" :class="p.session_state" aria-hidden="true" />
                <span class="gate-profile-text">
                  <span class="gate-profile-url">
                    {{ profileLabel(p) }}
                    <em v-if="p.active" class="gate-profile-tag">{{ t('native.profiles.lastUsed') }}</em>
                  </span>
                  <span class="gate-profile-meta">
                    {{ p.server_url }}<template v-if="p.bridge_id"> · {{ p.bridge_id }}</template>
                  </span>
                </span>
                <span class="gate-profile-badge" :class="p.session_state">{{ t(profileBadgeKey(p)) }}</span>
                <span class="gate-profile-cta">
                  {{ p.session_state === 'active' ? t('native.profiles.resume') : t('native.profiles.reconnect') }}
                </span>
              </button>
              <button
                type="button"
                class="gate-profile-delete"
                :aria-label="t('native.profiles.deleteTitle')"
                :disabled="deletingProfileId !== null || pendingProfileId !== null"
                @click="armDelete(p)"
              >
                <Trash2 :size="14" aria-hidden="true" />
              </button>
            </div>
            <div v-if="confirmingDeleteId === p.id" data-testid="profile-delete-confirm" class="gate-profile-confirm">
              <span class="gate-profile-confirm-text">
                {{ t('native.profiles.deleteBody', { label: profileLabel(p) }) }}
              </span>
              <span class="gate-profile-confirm-actions">
                <button type="button" class="gate-ghost" @click="cancelDelete">
                  {{ t('common.cancel') }}
                </button>
                <button
                  type="button"
                  class="gate-profile-confirm-delete"
                  :disabled="deletingProfileId !== null"
                  @click="doDelete(p)"
                >
                  <LoaderCircle v-if="deletingProfileId === p.id" class="gate-spinner" :size="13" aria-hidden="true" />
                  {{ t('native.profiles.deleteConfirm') }}
                </button>
              </span>
            </div>
          </li>
        </ul>
        <p v-if="profileNote" class="gate-profile-note" role="status">{{ profileNote }}</p>
      </div>

      <!-- Bridge selection (entry ②): the edge fronts several bridges. -->
      <div v-if="step === 'bridges'" data-testid="bridge-step" class="gate-form">
        <p class="gate-step-title">{{ t('native.chooseBridge') }}</p>
        <p class="gate-sub">{{ t('native.chooseBridgeHint') }}</p>
        <ul class="gate-bridges">
          <li v-for="b in bridges" :key="b.bridge_id">
            <button type="button" class="gate-bridge" @click="pickBridge(b.bridge_id)">
              <MonitorSmartphone :size="16" aria-hidden="true" />
              <code>{{ b.bridge_id }}</code>
              <span class="gate-bridge-live" :class="{ live: b.live }">
                {{ b.live ? '● online' : '● offline' }}
              </span>
            </button>
          </li>
        </ul>
        <p v-if="error" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ error }}
        </p>
        <button type="button" class="gate-back" @click="backToForm">{{ t('native.backToForm') }}</button>
      </div>

      <!-- Embedded-bridge enrollment (machine plane): three fields — the
           edge URL, the one-time enrollment token, and the user's API
           key. Separated from the remote-connection tabs on purpose:
           this flow registers THIS machine's bridge; the tabs connect to
           someone else's. -->
      <div v-if="step === 'enroll'" data-testid="enroll-step" class="gate-form">
        <p class="gate-step-title">{{ t('native.enroll.title') }}</p>
        <p class="gate-sub">{{ t('native.enroll.subtitle') }}</p>
        <form @submit.prevent="submitEnroll">
          <label>
            {{ t('native.enroll.edgeUrl') }}
            <span class="gate-field">
              <Globe :size="16" aria-hidden="true" />
              <input
                v-model="enrollUrl"
                data-testid="enroll-url"
                type="url"
                autocomplete="url"
                :placeholder="t('native.enroll.edgePlaceholder')"
                spellcheck="false"
              />
            </span>
          </label>
          <label>
            {{ t('native.enroll.tokenLabel') }}
            <span class="gate-field">
              <CloudUpload :size="16" aria-hidden="true" />
              <input
                v-model="enrollToken"
                data-testid="enroll-token"
                type="password"
                autocomplete="off"
                :placeholder="t('native.enroll.tokenPlaceholder')"
              />
            </span>
          </label>
          <p class="gate-hint">{{ t('native.enroll.tokenHint') }}</p>
          <label>
            {{ t('native.apiKeyLabel') }}
            <span class="gate-field">
              <KeyRound :size="16" aria-hidden="true" />
              <input
                v-model="enrollKey"
                data-testid="enroll-key"
                type="password"
                autocomplete="off"
                :placeholder="t('native.apiKeyPlaceholder')"
              />
            </span>
          </label>
          <p v-if="enrollValidation" data-testid="enroll-validation" class="gate-alert" role="alert">
            <TriangleAlert :size="14" aria-hidden="true" />
            {{ enrollValidation }}
          </p>
          <p v-if="enrollError" data-testid="enroll-error" class="gate-alert" role="alert">
            <TriangleAlert :size="14" aria-hidden="true" />
            {{ enrollError }}
          </p>
          <button type="submit" class="gate-submit" data-testid="enroll-submit" :disabled="enrollPending">
            <LoaderCircle v-if="enrollPending" class="gate-spinner" :size="15" aria-hidden="true" />
            {{ enrollPending ? t('native.enroll.working') : t('native.enroll.submit') }}
          </button>
        </form>
        <button type="button" class="gate-back" @click="backToForm">{{ t('native.backToForm') }}</button>
      </div>

      <!-- Registered sign-in (user plane): the machine is enrolled, only
           the caller's identity is missing. The token field is gone — a
       consumed one-time token would be a dead end, not an input. -->
      <div v-else-if="step === 'enroll-login'" data-testid="enroll-login-step" class="gate-form">
        <p class="gate-step-title">{{ t('native.enroll.loginTitle') }}</p>
        <p class="gate-sub">{{ t('native.enroll.loginHint', { url: cloud?.edge_url ?? '' }) }}</p>
        <form @submit.prevent="submitEnrollLogin">
          <label>
            {{ t('native.apiKeyLabel') }}
            <span class="gate-field">
              <KeyRound :size="16" aria-hidden="true" />
              <input
                v-model="loginKey"
                data-testid="enroll-login-key"
                type="password"
                autocomplete="off"
                :placeholder="t('native.apiKeyPlaceholder')"
              />
            </span>
          </label>
          <p v-if="loginError" data-testid="enroll-login-error" class="gate-alert" role="alert">
            <TriangleAlert :size="14" aria-hidden="true" />
            {{ loginError }}
          </p>
          <button type="submit" class="gate-submit" data-testid="enroll-login-submit" :disabled="loginPending">
            <LoaderCircle v-if="loginPending" class="gate-spinner" :size="15" aria-hidden="true" />
            {{ loginPending ? t('native.connecting') : t('native.enroll.login') }}
          </button>
        </form>
        <p class="gate-foot-note">{{ t('native.enroll.manageHint') }}</p>
        <button type="button" class="gate-back" @click="backToForm">{{ t('native.backToForm') }}</button>
      </div>

      <!-- Entries ②/③: one credential shape (URL + key) behind two
           semantic tabs — the same profile mechanism underneath. With
           saved connections above, the form is the "add new" path. -->
      <form v-else-if="step === 'form'" class="gate-form" @submit.prevent="submit">
        <p v-if="profiles.length" class="gate-step-title">{{ t('native.profiles.addNew') }}</p>
        <div class="gate-tabs" role="tablist">
          <button
            type="button"
            role="tab"
            data-testid="tab-private"
            :aria-selected="tab === 'private'"
            :class="{ active: tab === 'private' }"
            @click="tab = 'private'"
          >
            {{ t('native.tabPrivate') }}
          </button>
          <button
            type="button"
            role="tab"
            data-testid="tab-direct"
            :aria-selected="tab === 'direct'"
            :class="{ active: tab === 'direct' }"
            @click="tab = 'direct'"
          >
            {{ t('native.tabDirect') }}
          </button>
        </div>
        <label>
          {{ urlLabel }}
          <span class="gate-field">
            <Globe :size="16" aria-hidden="true" />
            <input
              v-model="serverUrl"
              data-testid="url-input"
              type="url"
              autocomplete="url"
              :placeholder="urlPlaceholder"
              spellcheck="false"
            />
          </span>
        </label>
        <label>
          {{ t('native.apiKeyLabel') }}
          <span class="gate-field">
            <KeyRound :size="16" aria-hidden="true" />
            <input
              ref="apiKeyInput"
              v-model="apiKey"
              data-testid="api-key-input"
              type="password"
              autocomplete="off"
              :placeholder="t('native.apiKeyPlaceholder')"
            />
          </span>
        </label>
        <p v-if="validation" data-testid="connection-validation" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ validation }}
        </p>
        <p v-if="error" class="gate-alert" role="alert">
          <TriangleAlert :size="14" aria-hidden="true" />
          {{ error }}
        </p>
        <button type="submit" class="gate-submit" :disabled="pending">
          <LoaderCircle v-if="pending" class="gate-spinner" :size="15" aria-hidden="true" />
          {{ pending ? t('native.connecting') : t('native.connect') }}
        </button>
      </form>

      <p class="gate-foot">
        <ShieldCheck aria-hidden="true" />
        {{ t('native.keyHint') }}
      </p>
    </section>
  </main>
</template>

<style scoped>
.gate-saved {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.gate-saved .gate-step-title {
  text-align: left;
}
.gate-profiles {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.gate-profile-row {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.gate-profile {
  display: flex;
  align-items: stretch;
  gap: 6px;
}
.gate-profile-main {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  text-align: left;
}
.gate-profile-main:hover:not(:disabled) {
  border-color: var(--brand);
}
.gate-profile-main:disabled {
  opacity: 0.6;
  cursor: default;
}
.gate-profile-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted);
  flex: none;
}
.gate-profile-dot.active {
  background: var(--ok);
}
.gate-profile-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.gate-profile-url {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  font-weight: 600;
}
.gate-profile-tag {
  font-size: 10.5px;
  font-weight: 400;
  font-style: normal;
  color: #9aa3b2;
  border: 1px solid var(--border);
  border-radius: 999px;
  padding: 0 6px;
}
.gate-profile-meta {
  font-size: 11.5px;
  color: #9aa3b2;
  font-family: ui-monospace, monospace;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gate-profile-badge {
  flex: none;
  font-size: 11.5px;
  color: #9aa3b2;
  border: 1px solid var(--border);
  border-radius: 999px;
  padding: 1px 8px;
}
.gate-profile-badge.active {
  border-color: transparent;
  background: var(--brand);
  color: #fff;
}
.gate-profile-cta {
  flex: none;
  font-size: 12.5px;
  color: var(--brand-hover);
}
.gate-profile-delete {
  flex: none;
  width: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: transparent;
  color: #9aa3b2;
  cursor: pointer;
}
.gate-profile-delete:hover:not(:disabled) {
  color: var(--danger);
  border-color: color-mix(in srgb, var(--danger) 45%, var(--border));
}
.gate-profile-delete:disabled {
  opacity: 0.6;
  cursor: default;
}
.gate-profile-confirm {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 8px 12px;
  border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--border));
  border-radius: 10px;
  font-size: 12px;
}
.gate-profile-confirm-text {
  flex: 1;
  min-width: 200px;
  color: var(--muted);
}
.gate-profile-confirm-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.gate-profile-confirm-delete {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: var(--danger);
  color: #fff;
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
}
.gate-profile-confirm-delete:disabled {
  opacity: 0.6;
  cursor: default;
}
.gate-profile-note {
  margin: 0;
  font-size: 12px;
  color: var(--muted);
  text-align: left;
}
.gate-embedded {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: rgba(79, 124, 255, 0.06);
}
.gate-embedded.enrolled {
  background: transparent;
}
.gate-embedded-text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 12.5px;
}
.gate-embedded-text span {
  color: #9aa3b2;
  font-size: 11.5px;
}
.gate-embedded-id {
  font-family: ui-monospace, monospace;
  font-size: 11px;
  margin-left: 6px;
  opacity: 0.8;
}
.gate-embedded-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: none;
}
.gate-embedded-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 7px 12px;
  border-radius: 8px;
  border: 1px solid var(--brand);
  background: transparent;
  color: var(--brand);
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
  white-space: nowrap;
}
.gate-embedded-btn:hover {
  background: rgba(79, 124, 255, 0.12);
}
.gate-embedded-btn--secondary {
  border-color: var(--border);
  color: inherit;
}
.gate-embedded-btn--secondary:hover {
  border-color: var(--brand);
  color: var(--brand);
  background: transparent;
}
.gate-embedded-btn--danger {
  border-color: color-mix(in srgb, var(--danger) 45%, var(--border));
  color: var(--danger-text, var(--danger));
}
.gate-embedded-btn--danger:hover {
  background: transparent;
  border-color: var(--danger);
}
.gate-hint,
.gate-foot-note {
  margin: -6px 0 0;
  font-size: 11.5px;
  color: #9aa3b2;
}
.gate-foot-note {
  margin: 0;
  text-align: center;
}
.gate-tabs {
  display: flex;
  gap: 6px;
  padding: 4px;
  border: 1px solid var(--border);
  border-radius: 10px;
}
.gate-tabs button {
  flex: 1;
  padding: 7px 0;
  border: none;
  border-radius: 7px;
  background: transparent;
  color: #9aa3b2;
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
}
.gate-tabs button.active {
  background: rgba(79, 124, 255, 0.16);
  color: var(--brand-hover);
}
.gate-step-title {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}
.gate-bridges {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.gate-bridge {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  font-size: 13px;
}
.gate-bridge:hover {
  border-color: var(--brand);
}
.gate-bridge code {
  flex: 1;
  text-align: left;
  font-size: 12.5px;
}
.gate-bridge-live {
  color: var(--muted);
  font-size: 11.5px;
}
.gate-bridge-live.live {
  color: var(--ok);
}
.gate-back {
  align-self: center;
  border: none;
  background: none;
  color: var(--brand-hover);
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
  padding: 0;
}
</style>
