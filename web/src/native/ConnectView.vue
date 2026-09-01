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
  Server,
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
import InkWaveBackground from '../components/InkWaveBackground.vue'
import { validateEnterpriseOrigin } from './origin'

const props = defineProps<{
  serverUrl?: string
  authorizationRequired?: boolean
  /** Entry ① (wails only): the embedded bridge's base URL. */
  embeddedUrl?: string
  connect: (input: ConnectionInput) => Promise<unknown>
  /** One-tap resume of a saved profile along its stored session. */
  resume?: (id: number) => Promise<unknown>
  /** Embedded-bridge enrollment (the three-field registration page). */
  enroll?: (input: EnrollEmbeddedInput) => Promise<EnrollResult>
  /** Deregister the embedded bridge (back to local mode). */
  deregister?: () => Promise<EnrollResult>
}>()

const emit = defineEmits<{ 'connect-embedded': [] }>()

const { t } = useI18n()

/* ------------------------------ page machine ------------------------------ *
 * The wizard (login-journey spec): every page answers exactly one question.
 *   P0 mode        — local or remote (plus the returning-user hero)
 *   P1 remote way  — private cloud or direct bridge
 *   P2A/P2B forms  — URL + key (per flavor)
 *   P3 bridges     — pick one when the edge fronts several
 *   P4 enrollment  — machine-plane registration (three fields)
 *   P5 local login — single key for an enrolled embedded bridge
 * Forward moves rise; going back is a plain fade (asymmetric on purpose). */
type Page = 'p0' | 'p1' | 'p2a' | 'p2b' | 'p3' | 'p4' | 'p5'
const page = ref<Page>('p0')
const navForward = ref(true)
function goto(next: Page, back = false) {
  if (next === page.value) return
  navForward.value = !back
  page.value = next
}
function backTo(target: Page) {
  goto(target, true)
}

const serverUrl = ref(props.serverUrl ?? '')
const apiKey = ref('')
const validation = ref('')
const error = ref('')
const pending = ref(false)
// Bridge selection (P3): the edge fronts several bridges.
const bridges = ref<EdgeBridge[]>([])

// Saved connections (the login history): profiles with a live session rise
// to the P0 hero card (one-tap resume); expired ones stay in the list and
// prefill the matching form page. The listing is optional surface on older
// binaries — absence is not an error.
const native = getNativeBridge()
const profiles = ref<ProfileSummary[]>([])
const pendingProfileId = ref<number | null>(null)
const deletingProfileId = ref<number | null>(null)
const confirmingDeleteId = ref<number | null>(null)
const profileNote = ref('')
const apiKeyInput = ref<HTMLInputElement | null>(null)

const heroProfile = computed(() => profiles.value.find((p) => p.session_state === 'active') ?? null)
const listedProfiles = computed(() => profiles.value.filter((p) => p.session_state !== 'active'))

// The embedded bridge's enrollment state (optional surface — absent on
// dev builds and older binaries). Drives the local-mode card's shape: the
// zero-input local entry, the registration link, or the enrolled badge
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

// A direct link (https://edge/?bridge=<id>) skips the wizard and lands on
// the private-cloud form with the bridge pinned up front.
onMounted(() => {
  const raw = (props.serverUrl ?? '').trim()
  if (raw.includes('?')) {
    serverUrl.value = raw
    goto('p2a')
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
  // Expired or signed out: prefill the matching form page (a stored
  // bridge_id means it came through an edge → the private-cloud page) and
  // focus the key.
  serverUrl.value = p.server_url
  validation.value = ''
  error.value = ''
  goto(p.bridge_id ? 'p2a' : 'p2b')
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

const urlLabel = computed(() =>
  page.value === 'p2b' ? t('native.urlLabelDirect') : t('native.urlLabelPrivate'),
)
const urlPlaceholder = computed(() =>
  page.value === 'p2b' ? t('native.urlPlaceholderDirect') : t('native.urlPlaceholderPrivate'),
)

watch(
  () => props.serverUrl,
  (value) => {
    serverUrl.value = value ?? ''
  },
)
watch(page, () => {
  // Page transitions clear only their own page's errors — a class-token
  // enrollment failure (e.g. a rejected key routing to sign-in) carries
  // its message across the move.
  if (page.value !== 'p4') enrollValidation.value = ''
  if (page.value !== 'p5') loginError.value = ''
  if (page.value !== 'p3') error.value = ''
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
  enrollError.value = ''
  enrollValidation.value = ''
  goto('p4')
}

function openEnrollLogin() {
  loginError.value = ''
  goto('p5')
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
      enrollToken.value = ''
      enrollKey.value = ''
      goto('p5')
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
    // Success: back to the unenrolled landing page.
    confirmingDeregister.value = false
    cloud.value = { available: true, enrolled: false }
    goto('p0', true)
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
    // P2A semantics: an edge may front several bridges — ask for the
    // pre-auth inventory and let the user pick. Unavailable inventories
    // (older binaries, non-native) fall through to a plain connect; a
    // single bridge needs no choice (the edge auto-routes).
    if (page.value === 'p2a') {
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
            goto('p3')
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
  goto('p2a', true)
  bridges.value = []
  error.value = ''
}
</script>

<template>
  <main class="native-gate">
    <InkWaveBackground ink />

    <section class="gate-card">
      <Transition :name="navForward ? 'wiz-fwd' : 'wiz-back'" mode="out-in">
        <div :key="page" class="wiz-page">

          <!-- ════════ P0 · mode choice (the landing page) ════════ -->
          <div v-if="page === 'p0'" class="wiz-inner">
            <div class="gate-brand gate-rise" style="--d: 80ms">
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

            <p v-if="authorizationRequired" class="gate-notice gate-rise" style="--d: 100ms">
              <TriangleAlert :size="15" aria-hidden="true" />
              {{ t('native.authorizationRequired') }}
            </p>
            <p v-if="error" class="gate-alert gate-rise" style="--d: 100ms" role="alert">
              <TriangleAlert :size="14" aria-hidden="true" />
              {{ error }}
            </p>

            <!-- Saved connections: the active profile rises to a one-tap
                 hero above the mode cards; the rest stay a quiet list that
                 prefills the matching form. -->
            <div v-if="profiles.length" data-testid="saved-profiles" class="gate-saved gate-rise" style="--d: 120ms">
              <template v-if="heroProfile">
                <div class="gate-profile">
                  <button
                    type="button"
                    class="gate-profile-main gate-hero"
                    :disabled="pendingProfileId !== null || deletingProfileId !== null"
                    @click="useProfile(heroProfile)"
                  >
                    <LoaderCircle v-if="pendingProfileId === heroProfile.id" class="gate-spinner" :size="16" aria-hidden="true" />
                    <span v-else class="gate-profile-dot active" aria-hidden="true" />
                    <span class="gate-profile-text">
                      <span class="gate-profile-url">
                        {{ profileLabel(heroProfile) }}
                        <em class="gate-profile-tag">{{ t('native.profiles.lastUsed') }}</em>
                      </span>
                      <span class="gate-profile-meta">
                        {{ heroProfile.server_url }}<template v-if="heroProfile.bridge_id"> · {{ heroProfile.bridge_id }}</template>
                      </span>
                    </span>
                    <span class="gate-profile-badge active">{{ t('native.profiles.signedIn') }}</span>
                    <span class="gate-hero-cta">{{ t('native.profiles.resume') }}</span>
                  </button>
                  <button
                    type="button"
                    class="gate-profile-delete"
                    :aria-label="t('native.profiles.deleteTitle')"
                    :disabled="deletingProfileId !== null || pendingProfileId !== null"
                    @click="armDelete(heroProfile)"
                  >
                    <Trash2 :size="14" aria-hidden="true" />
                  </button>
                </div>
                <div v-if="confirmingDeleteId === heroProfile.id" data-testid="profile-delete-confirm" class="gate-profile-confirm">
                  <span class="gate-profile-confirm-text">
                    {{ t('native.profiles.deleteBody', { label: profileLabel(heroProfile) }) }}
                  </span>
                  <span class="gate-profile-confirm-actions">
                    <button type="button" class="gate-ghost gate-ghost--sm" @click="cancelDelete">
                      {{ t('common.cancel') }}
                    </button>
                    <button
                      type="button"
                      class="gate-profile-confirm-delete"
                      :disabled="deletingProfileId !== null"
                      @click="doDelete(heroProfile)"
                    >
                      <LoaderCircle v-if="deletingProfileId === heroProfile.id" class="gate-spinner" :size="13" aria-hidden="true" />
                      {{ t('native.profiles.deleteConfirm') }}
                    </button>
                  </span>
                </div>
              </template>
              <p v-if="listedProfiles.length" class="gate-section-title">{{ t('native.profiles.title') }}</p>
              <ul v-if="listedProfiles.length" class="gate-profiles">
                <li v-for="p in listedProfiles" :key="p.id" class="gate-profile-row">
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
                        <span class="gate-profile-url">{{ profileLabel(p) }}</span>
                        <span class="gate-profile-meta">
                          {{ p.server_url }}<template v-if="p.bridge_id"> · {{ p.bridge_id }}</template>
                        </span>
                      </span>
                      <span class="gate-profile-badge" :class="p.session_state">{{ t(profileBadgeKey(p)) }}</span>
                      <span class="gate-profile-cta">
                        {{ t('native.profiles.reconnect') }}
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
                      <button type="button" class="gate-ghost gate-ghost--sm" @click="cancelDelete">
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

            <!-- Local mode: the embedded bridge — same mode-card anatomy as
                 the remote card below. Enrolled swaps in the badge and the
                 sign-in exit; enrollment itself lives on the remote page
                 (it is a cloud-side relationship, not a local-mode action). -->
            <button
              v-if="embeddedUrl"
              type="button"
              data-testid="embedded-entry"
              class="gate-mode-card gate-rise"
              :class="{ enrolled: cloud?.enrolled }"
              style="--d: 160ms"
              @click="cloud?.enrolled ? openEnrollLogin() : useEmbedded()"
            >
              <span class="gate-mode-icon"><HardDrive :size="19" aria-hidden="true" /></span>
              <span class="gate-mode-text">
                <span class="gate-mode-title-row">
                  <span class="gate-mode-title">{{ t('native.embeddedTitle') }}</span>
                  <span v-if="cloud?.enrolled" class="gate-mode-badge">{{ t('native.enroll.registeredBadge') }}</span>
                </span>
                <span class="gate-mode-hint">{{ cloud?.enrolled ? t('native.wizard.embeddedEnrolledHint') : t('native.embeddedHint') }}</span>
                <span v-if="cloud?.enrolled && cloud.bridge_id" class="gate-mode-sub">{{ cloud.bridge_id }}</span>
              </span>
              <span v-if="!cloud?.enrolled" data-testid="embedded-connect" class="gate-mode-cta">{{ t('native.embeddedUse') }} →</span>
              <span v-else data-testid="embedded-signin" class="gate-mode-cta">{{ t('native.enroll.login') }} →</span>
            </button>
            <p v-if="deregisterNote" class="gate-profile-note" role="status">{{ deregisterNote }}</p>

            <!-- Remote mode: the second axis choice. -->
            <button type="button" data-testid="remote-open" class="gate-mode-card gate-rise" style="--d: 200ms" @click="goto('p1')">
              <span class="gate-mode-icon"><Server :size="19" aria-hidden="true" /></span>
              <span class="gate-mode-text">
                <span class="gate-mode-title">{{ t('native.wizard.remoteTitle') }}</span>
                <span class="gate-mode-hint">{{ t('native.wizard.remoteHint') }}</span>
              </span>
              <span class="gate-mode-cta">{{ t('native.wizard.remoteCta') }}</span>
            </button>

            <p class="gate-foot gate-rise" style="--d: 240ms">
              <ShieldCheck aria-hidden="true" />
              {{ t('native.keyHint') }}
            </p>
          </div>

          <!-- ════════ P1 · remote way ════════ -->
          <div v-else-if="page === 'p1'" class="wiz-inner">
            <div class="gate-page-head gate-rise" style="--d: 0ms">
              <button type="button" class="gate-back-btn" :aria-label="t('native.wizard.back')" @click="backTo('p0')">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6" /></svg>
              </button>
              <span class="gate-page-title">{{ t('native.wizard.remoteTitle') }}</span>
            </div>
            <p class="gate-sub gate-rise" style="--d: 40ms">{{ t('native.wizard.chooseSubtitle') }}</p>

            <button type="button" data-testid="tab-private" class="gate-mode-card gate-rise" style="--d: 80ms" @click="goto('p2a')">
              <span class="gate-mode-icon"><CloudUpload :size="19" aria-hidden="true" /></span>
              <span class="gate-mode-text">
                <span class="gate-mode-title">{{ t('native.tabPrivate') }}</span>
                <span class="gate-mode-hint">{{ t('native.wizard.privateHint') }}</span>
              </span>
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="gate-mode-chev"><path d="m9 18 6-6-6-6" /></svg>
            </button>

            <button type="button" data-testid="tab-direct" class="gate-mode-card gate-rise" style="--d: 120ms" @click="goto('p2b')">
              <span class="gate-mode-icon"><MonitorSmartphone :size="19" aria-hidden="true" /></span>
              <span class="gate-mode-text">
                <span class="gate-mode-title">{{ t('native.tabDirect') }}</span>
                <span class="gate-mode-hint">{{ t('native.wizard.directHint') }}</span>
              </span>
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="gate-mode-chev"><path d="m9 18 6-6-6-6" /></svg>
            </button>

            <!-- The machine-plane relationship lives here, on the remote
                 side: register this machine's bridge to a private cloud —
                 or, once enrolled, break that relationship. Two-step
                 confirm inline (the gate page has no app dialog store). -->
            <div v-if="embeddedUrl && !cloud?.enrolled" class="gate-card-links gate-rise" style="--d: 160ms">
              <button type="button" data-testid="embedded-enroll-open" class="gate-quiet-link" @click="openEnroll">
                <CloudUpload :size="13" aria-hidden="true" />
                {{ t('native.enroll.open') }}
              </button>
            </div>
            <div v-if="embeddedUrl && cloud?.enrolled" class="gate-card-links gate-rise" style="--d: 160ms">
              <button type="button" data-testid="embedded-deregister-arm" class="gate-quiet-link gate-quiet-link--danger" @click="confirmingDeregister = true">
                {{ t('native.enroll.deregister') }}
              </button>
            </div>
            <div v-if="cloud?.enrolled && confirmingDeregister" data-testid="embedded-deregister-confirm" class="gate-profile-confirm gate-rise" style="--d: 180ms">
              <span class="gate-profile-confirm-text">{{ t('native.enroll.deregisterBody') }}</span>
              <span class="gate-profile-confirm-actions">
                <button type="button" class="gate-ghost gate-ghost--sm" @click="confirmingDeregister = false">
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
          </div>

          <!-- ════════ P2A / P2B · credential forms ════════ -->
          <form v-else-if="page === 'p2a' || page === 'p2b'" class="wiz-inner" @submit.prevent="submit">
            <div class="gate-page-head gate-rise" style="--d: 0ms">
              <button type="button" class="gate-back-btn" :aria-label="t('native.wizard.back')" @click="backTo('p1')">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6" /></svg>
              </button>
              <span class="gate-page-title">
                {{ page === 'p2b' ? t('native.wizard.formDirectTitle') : t('native.wizard.formPrivateTitle') }}
              </span>
            </div>
            <p class="gate-sub gate-rise" style="--d: 40ms">
              {{ page === 'p2b' ? t('native.wizard.formDirectSubtitle') : t('native.wizard.formPrivateSubtitle') }}
            </p>
            <div class="gate-rise" style="--d: 80ms">
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
            </div>
          </form>

          <!-- ════════ P3 · bridge selection ════════ -->
          <div v-else-if="page === 'p3'" data-testid="bridge-step" class="wiz-inner">
            <div class="gate-page-head gate-rise" style="--d: 0ms">
              <button type="button" class="gate-back-btn" :aria-label="t('native.backToForm')" @click="backToForm">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6" /></svg>
              </button>
              <span class="gate-page-title">{{ t('native.chooseBridge') }}</span>
            </div>
            <p class="gate-sub gate-rise" style="--d: 40ms">{{ t('native.chooseBridgeHint') }}</p>
            <ul class="gate-bridges gate-rise" style="--d: 80ms">
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
          </div>

          <!-- ════════ P4 · enrollment (machine plane) ════════ -->
          <div v-else-if="page === 'p4'" data-testid="enroll-step" class="wiz-inner">
            <div class="gate-page-head gate-rise" style="--d: 0ms">
              <button type="button" class="gate-back-btn" :aria-label="t('native.wizard.back')" @click="backTo('p0')">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6" /></svg>
              </button>
              <span class="gate-page-title">{{ t('native.enroll.title') }}</span>
            </div>
            <p class="gate-sub gate-rise" style="--d: 40ms">{{ t('native.enroll.subtitle') }}</p>
            <form class="gate-rise" style="--d: 80ms" @submit.prevent="submitEnroll">
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
          </div>

          <!-- ════════ P5 · local sign-in (user plane) ════════ -->
          <div v-else-if="page === 'p5'" data-testid="enroll-login-step" class="wiz-inner">
            <div class="gate-page-head gate-rise" style="--d: 0ms">
              <button type="button" class="gate-back-btn" :aria-label="t('native.wizard.back')" @click="backTo('p0')">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6" /></svg>
              </button>
              <span class="gate-page-title">{{ t('native.enroll.loginTitle') }}</span>
            </div>
            <p class="gate-sub gate-rise" style="--d: 40ms">
              {{ t('native.enroll.loginHint', { url: cloud?.edge_url ?? '' }) }}
            </p>
            <form class="gate-rise" style="--d: 80ms" @submit.prevent="submitEnrollLogin">
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
            <p class="gate-foot-note gate-rise" style="--d: 120ms">{{ t('native.enroll.manageHint') }}</p>
          </div>

        </div>
      </Transition>
    </section>
  </main>
</template>

<style scoped>
/* Ink connect wizard — theme-invariant values from the --gate-ink-* group
   (tokens.css): the front door stays dark in either workspace theme. */
.wiz-page {
  display: block;
}

.gate-section-title {
  margin: 0 0 10px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--gate-ink-faint);
  text-align: left;
}
.gate-saved {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-bottom: 6px;
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
  border: 1px solid var(--gate-ink-border);
  border-radius: 10px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  text-align: left;
  transition: border-color var(--dur-fast) var(--ease-out);
}
.gate-profile-main:hover:not(:disabled) {
  border-color: var(--gate-ink-focus-border);
}
.gate-profile-main:disabled {
  opacity: 0.6;
  cursor: default;
}
.gate-hero {
  padding: 14px 16px;
  border-color: var(--gate-ink-border-strong);
  background: color-mix(in srgb, var(--gate-ink-text) 5%, transparent);
}
.gate-hero:hover:not(:disabled) {
  background: color-mix(in srgb, var(--gate-ink-text) 8%, transparent);
}
.gate-hero-cta {
  flex: none;
  font-size: 13px;
  font-weight: 600;
  color: var(--gate-ink-text);
}
.gate-profile-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--gate-ink-muted);
  flex: none;
}
.gate-profile-dot.active {
  background: var(--gate-ink-ok);
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
.gate-hero .gate-profile-url {
  font-size: 14.5px;
}
.gate-profile-tag {
  font-size: 10.5px;
  font-weight: 400;
  font-style: normal;
  color: var(--gate-ink-muted);
  border: 1px solid var(--gate-ink-border);
  border-radius: 999px;
  padding: 0 6px;
}
.gate-profile-meta {
  font-size: 11.5px;
  color: var(--gate-ink-muted);
  font-family: ui-monospace, monospace;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gate-profile-badge {
  flex: none;
  font-size: 11.5px;
  color: var(--gate-ink-muted);
  border: 1px solid var(--gate-ink-border);
  border-radius: 999px;
  padding: 1px 8px;
}
.gate-profile-badge.active {
  border-color: transparent;
  background: var(--gate-ink-accent);
  color: var(--gate-ink-on-accent);
}
.gate-profile-cta {
  flex: none;
  font-size: 12.5px;
  color: var(--gate-ink-text-2);
}
.gate-hero .gate-profile-cta {
  display: none;
}
.gate-profile-delete {
  flex: none;
  width: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--gate-ink-border);
  border-radius: 10px;
  background: transparent;
  color: var(--gate-ink-muted);
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
}
.gate-profile-delete:hover:not(:disabled) {
  color: var(--gate-ink-danger);
  border-color: color-mix(in srgb, var(--gate-ink-danger) 45%, transparent);
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
  border: 1px solid color-mix(in srgb, var(--gate-ink-danger) 40%, transparent);
  border-radius: 10px;
  font-size: 12px;
}
.gate-profile-confirm-text {
  flex: 1;
  min-width: 200px;
  color: var(--gate-ink-muted);
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
  background: var(--gate-ink-danger);
  color: var(--on-danger);
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
  color: var(--gate-ink-muted);
  text-align: left;
}

/* Quiet card-level links (the machine-plane entry on the remote page). */
.gate-card-links {
  display: flex;
  gap: 14px;
  padding: 0 4px;
  margin: 2px 0 10px;
}
.gate-quiet-link {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border: none;
  background: none;
  color: var(--gate-ink-muted);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  padding: 0;
  transition: color var(--dur-fast) var(--ease-out);
}
.gate-quiet-link:hover {
  color: var(--gate-ink-text-2);
}
.gate-quiet-link--danger:hover {
  color: var(--gate-ink-danger);
}
.gate-ghost--sm {
  margin-top: 0;
  width: auto;
  padding: 6px 12px;
  font-size: 12.5px;
}

/* Mode cards (P0 remote + P1 pair). */
.gate-mode-card {
  display: flex;
  align-items: center;
  gap: 14px;
  width: 100%;
  padding: 15px 16px;
  border: 1px solid var(--gate-ink-border);
  border-radius: 14px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  text-align: left;
  margin-bottom: 10px;
  transition:
    border-color var(--dur-fast) var(--ease-out),
    background var(--dur-fast) var(--ease-out);
}
.gate-mode-card:hover {
  border-color: var(--gate-ink-focus-border);
  background: color-mix(in srgb, var(--gate-ink-text) 4%, transparent);
}
.gate-mode-icon {
  width: 38px;
  height: 38px;
  border-radius: 10px;
  flex: none;
  display: grid;
  place-items: center;
  border: 1px solid var(--gate-ink-border-strong);
  color: var(--gate-ink-text-2);
}
.gate-mode-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.gate-mode-title-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.gate-mode-badge {
  font-size: 10px;
  font-weight: 600;
  padding: 1px 7px;
  border-radius: 999px;
  background: color-mix(in srgb, var(--gate-ink-text) 12%, transparent);
}
.gate-mode-sub {
  font-size: 11px;
  font-family: ui-monospace, monospace;
  color: var(--gate-ink-muted);
}
.gate-mode-title {
  font-size: 15px;
  font-weight: 600;
}
.gate-mode-hint {
  font-size: 12px;
  line-height: 1.5;
  color: var(--gate-ink-muted);
}
.gate-mode-cta {
  flex: none;
  font-size: 13px;
  font-weight: 600;
  color: var(--gate-ink-text-2);
}
.gate-mode-chev {
  flex: none;
  color: var(--gate-ink-faint);
}

/* Page head (P1-P5): back button + title express the hierarchy — no
   stepper (the branch tree is asymmetric, a progress bar would lie). */
.gate-page-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 4px;
}
.gate-back-btn {
  display: inline-grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border-radius: 8px;
  border: 1px solid var(--gate-ink-border);
  background: transparent;
  color: var(--gate-ink-muted);
  cursor: pointer;
  flex: none;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
}
.gate-back-btn:hover {
  color: var(--gate-ink-text);
  border-color: var(--gate-ink-focus-border);
}
.gate-page-title {
  font-size: 20px;
  font-weight: 700;
}

.gate-hint,
.gate-foot-note {
  margin: -6px 0 0;
  font-size: 11.5px;
  color: var(--gate-ink-muted);
}
.gate-foot-note {
  margin: 8px 0 0;
  text-align: center;
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
  border: 1px solid var(--gate-ink-border);
  border-radius: 10px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font: inherit;
  font-size: 13px;
  transition: border-color var(--dur-fast) var(--ease-out);
}
.gate-bridge:hover {
  border-color: var(--gate-ink-focus-border);
}
.gate-bridge code {
  flex: 1;
  text-align: left;
  font-size: 12.5px;
}
.gate-bridge-live {
  color: var(--gate-ink-muted);
  font-size: 11.5px;
}
.gate-bridge-live.live {
  color: var(--gate-ink-ok);
}
</style>
