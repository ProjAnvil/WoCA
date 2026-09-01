<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Activity, ShieldCheck,
  Brain,
  CloudDownload,
  CloudUpload,
  Cpu,
  Database,
  Globe,
  Lock,
  LogOut,
  Network,
  Palette,
  Plug,
  Puzzle,
  Radio,
  Server,
  Settings2,
  Unplug,
  X,
} from 'lucide-vue-next'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/settings'
import { useServerSettingsStore } from '../stores/serverSettings'
import { useStatusStore } from '../stores/status'
import { useClaudeEnvStore, useManagedStore, usePluginsStore } from '../stores/sections'
import { logout as apiLogout } from '../api/client'
import AppearanceSection from './settings/AppearanceSection.vue'
import PreferencesSection from './settings/PreferencesSection.vue'
import ProviderSection from './settings/ProviderSection.vue'
import ClaudeEnvSection from './settings/ClaudeEnvSection.vue'
import PluginsSection from './settings/PluginsSection.vue'
import StorageSection from './settings/StorageSection.vue'
import AdminSection from './settings/AdminSection.vue'
import CloudAccessSection from './settings/CloudAccessSection.vue'
import EdgeSwitchSection from './settings/EdgeSwitchSection.vue'
import NativeConnectSection from './settings/NativeConnectSection.vue'
import RemotesSection from './settings/RemotesSection.vue'
import { getNativeBridge } from '../native/bridge'
import ManagedSection from './settings/ManagedSection.vue'
import { getEdgeBridges, type EdgeBridge } from '../api/client'
import MessagePlaneSection from './settings/MessagePlaneSection.vue'
import MemorySection from './settings/MemorySection.vue'
import ModelSection from './settings/ModelSection.vue'
import StatsSection from './settings/StatsSection.vue'

const { t } = useI18n()
const settings = useSettingsStore()
const server = useServerSettingsStore()
const auth = useAuthStore()

// Admin gating for the cloud-access/admin sections: the core's role label
// is "tenant-admin" ('admin' kept for older bridges).
const isAdmin = computed(() => auth.me?.role === 'admin' || auth.me?.role === 'tenant-admin')

// Sidebar navigation: each entry maps to one section of the right pane.
const baseSections = [
  { id: 'appearance', label: 'settings.nav.appearance', icon: Palette },
  { id: 'preferences', label: 'settings.nav.preferences', icon: Settings2 },
  { id: 'provider', label: 'settings.nav.provider', icon: Server },
  { id: 'claudeenv', label: 'settings.nav.claudeenv', icon: Plug },
  { id: 'plugins', label: 'settings.nav.plugins', icon: Puzzle },
  { id: 'storage', label: 'settings.nav.storage', icon: Database },
  { id: 'msgplane', label: 'settings.nav.msgplane', icon: Radio },
  { id: 'memory', label: 'settings.nav.memory', icon: Brain },
  { id: 'model', label: 'settings.nav.model', icon: Cpu },
  { id: 'stats', label: 'settings.nav.stats', icon: Activity },
]
// The message plane is an enterprise feature: the nav entry appears only
// when the server reports a dispatching deployment.
const sections = computed(() => [
  ...baseSections.slice(0, 4), // through claudeenv
  // The 远端配置 page (config-plane delivery, read-only): bridge only —
  // personal and plain-server editions have no config-plane channel.
  ...(statusInfo.value?.edition === 'bridge'
    ? [{ id: 'managed', label: 'settings.nav.managed', icon: CloudDownload }]
    : []),
  ...baseSections.slice(4, 6), // plugins, storage
  ...(server.server?.msg_plane_available
    ? [{ id: 'msgplane', label: 'settings.nav.msgplane', icon: Radio }]
    : []),
  ...baseSections.slice(7), // memory, model, stats
  // The bridge switcher (接入点) appears only behind an edge access
  // point — the probe answers null on direct-bridge and personal
  // origins, so nothing else changes shape.
  ...(edgeBridges.value?.length
    ? [{ id: 'edge', label: 'settings.nav.edge', icon: Network }]
    : []),
  // Remote profiles (接入点切换, bridge 形态): direct-bridge origins keep
  // their own saved remote list in the bridge's settings database.
  ...(isBridge.value && !edgeBridges.value?.length
    ? [{ id: 'remotes', label: 'settings.nav.remotes', icon: Globe }]
    : []),
  // The native-only connection switcher: web editions switch bridges via
  // the access-point section above; the app switches profiles via its
  // own connect view, and this entry is the way back to it.
  ...(getNativeBridge()
    ? [{ id: 'nativeconnect', label: 'settings.nav.nativeConnect', icon: Plug }]
    : []),
  // The bridge cloud-access page (edge URL / enrollment, §6.5) and the
  // read-only admin page (sessions/audit — v1.7: no key management on
  // the bridge) — admin keys only. The core issues roles as
  // tenant-admin/member/viewer; 'admin' is kept for older bridges.
  ...(isAdmin.value || statusAuth.value === 'local'
    ? [{ id: 'cloudaccess', label: 'settings.nav.cloudaccess', icon: CloudUpload }]
    : []),
  ...(isAdmin.value ? [{ id: 'admin', label: 'settings.nav.admin', icon: ShieldCheck }] : []),
])

const active = ref<string>('appearance')

// The native app switches sessions through its own binding: the web
// logout route only revokes the server session, and the reload's
// Bootstrap would re-open along the vault's stored copy — never reaching
// the connect page. The native Logout drops that copy (profiles survive)
// and the reload lands on the connect page with the saved-connection
// list. The capability probe is the edition seam — same pattern as the
// nav gating above.
const native = getNativeBridge()

// The bridge edition's model-provider domain is cloud-governed (the catalog
// pull + verify is the selection surface; keys never live user-side), and
// on apikey bridges only admins hold settings:write — so members would face
// editable fields that can only fail on save. Gray the section instead.
const providerLocked = computed(
  () =>
    statusInfo.value?.edition === 'bridge' &&
    (statusInfo.value.managed || Boolean(statusInfo.value?.cloud?.enrolled)) &&
    statusAuth.value !== 'local',
)
const isBridge = computed(() => statusInfo.value?.edition === 'bridge')

// Logout rides the same-origin auth route: the bridge revokes at the core,
// the edge intercepts its own — the reload lands on the login gate either
// way. Only deployments with sessions (apikey posture) show it.
const canLogout = computed(() => Boolean(auth.me) || statusAuth.value === 'apikey')
const loggingOut = ref(false)
async function doLogout() {
  if (loggingOut.value) return
  loggingOut.value = true
  if (native) {
    try {
      await native.Logout()
    } catch {
      // Local cleanup already ran; the reload decides the gate.
    }
    window.location.reload()
    return
  }
  try {
    await apiLogout()
  } catch {
    // A dead channel still logged out locally; the reload decides the gate.
  }
  auth.me = null
  window.location.reload()
}

// The identity card's role badge: localized labels for the known roles,
// the raw value for anything a newer backend might introduce.
const roleLabel = computed(() => {
  const role = auth.me?.role
  if (!role) return ''
  if (role === 'admin' || role === 'tenant-admin') return t('settings.roles.admin')
  if (role === 'member') return t('settings.roles.member')
  if (role === 'viewer') return t('settings.roles.viewer')
  return role
})

// The cloud-access section follows the auth posture (§6.6): admin keys on
// apikey bridges, and every local-trust bridge — the embedded default is
// configured from the wails app by its owner.
const statusStore = useStatusStore()
const statusInfo = computed(() => statusStore.status)
const statusAuth = computed<'' | 'local' | 'apikey'>(() => (statusStore.status?.auth as '' | 'local' | 'apikey') ?? '')
// Edge inventory probe: non-null only when this origin is an edge access
// point (the bridge-switch section's gate).
const edgeBridges = ref<EdgeBridge[] | null>(null)
onMounted(async () => {
  await statusStore.refresh()
  edgeBridges.value = await getEdgeBridges()
})

// The access-point section's refresh re-probes the edge inventory in place.
async function refreshEdgeBridges() {
  edgeBridges.value = await getEdgeBridges()
}

// Suggested model names — the inputs are free-form; these are datalist hints.
const modelSuggestions = [
  'claude-sonnet-4-5',
  'claude-haiku-4-5',
  'claude-opus-4-5',
  'gpt-5.1',
  'gpt-4o',
  'gpt-4o-mini',
  'glm-4.6',
  'glm-5',
  'deepseek-chat',
  'deepseek-reasoner',
]

// Closing the panel resets the section stores: their fetched state is
// only meant to survive section switches while the panel stays open.
onBeforeUnmount(() => {
  useClaudeEnvStore().reset()
  usePluginsStore().reset()
  useManagedStore().reset()
})
</script>

<template>
  <div class="overlay" @click.self="settings.closePanel()">
    <div class="card settings-card">
      <div class="card-head">
        <span class="card-title">{{ t('settings.title') }}</span>
        <button class="icon-btn" @click="settings.closePanel()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body settings-layout">
        <!-- Left sidebar: section navigation -->
        <nav class="settings-nav">
          <button
            v-for="s in sections"
            :key="s.id"
            class="settings-nav-item"
            :class="{ active: active === s.id }"
            @click="active = s.id"
          >
            <component :is="s.icon" :size="14" />
            <span>{{ t(s.label) }}</span>
          </button>

          <!-- Bottom-left identity + logout (bridge/public-web sessions):
               the embedded personal edition has no session to end. The
               native app shows the same action as the connect section —
               disconnecting back to the connect page. -->
          <div v-if="canLogout" class="settings-nav-footer">
            <div class="nav-identity">
              <span class="nav-identity-name">{{ auth.me?.remark || auth.me?.user_id }}</span>
              <span v-if="roleLabel" class="nav-identity-role">{{ roleLabel }}</span>
            </div>
            <button
              class="settings-nav-item nav-logout"
              :disabled="loggingOut"
              @click="doLogout()"
            >
              <Unplug v-if="native" :size="14" />
              <LogOut v-else :size="14" />
              <span>{{ native ? t('settings.nativeConnect.button') : t('settings.logout') }}</span>
            </button>
          </div>
        </nav>

        <!-- Right pane: the selected section's fields -->
        <div class="settings-content">
          <div v-if="server.managedFields.length" class="managed-banner" role="note">
            <Lock :size="13" class="managed-banner-lock" />
            <span class="managed-banner-text">{{ t('settings.managedBanner') }}</span>
            <button
              v-if="isBridge"
              type="button"
              class="managed-banner-link"
              @click="active = 'managed'"
            >
              {{ t('settings.managedBannerLink') }}
            </button>
          </div>
          <!-- Section cross-fade: one keyed host so switching nav entries
               dissolves between sections instead of hard-cutting. -->
          <Transition name="fade" mode="out-in">
            <div :key="active" class="settings-section-host">
              <AppearanceSection v-if="active === 'appearance'" />
              <PreferencesSection v-if="active === 'preferences'" />
              <ProviderSection v-if="active === 'provider'" :locked="providerLocked" @navigate="active = $event" />
              <ClaudeEnvSection v-if="active === 'claudeenv'" :bridge="isBridge" />
              <ManagedSection v-if="active === 'managed'" :status="statusInfo" />
              <PluginsSection v-if="active === 'plugins'" :bridge="isBridge" />
              <StorageSection v-if="active === 'storage'" :bridge="isBridge" />
              <!-- Message plane is not a private-cloud concept (product design
                   §2 rule 2): the section disappears on managed bridges. -->
              <MessagePlaneSection
                v-if="active === 'msgplane' && server.server?.msg_plane_available && !server.managedFields.length"
              />
              <MemorySection v-if="active === 'memory'" />
              <ModelSection v-if="active === 'model'" />
              <StatsSection v-if="active === 'stats'" />
              <CloudAccessSection v-if="active === 'cloudaccess'" />
              <AdminSection v-if="active === 'admin'" />
              <EdgeSwitchSection
                v-if="active === 'edge'"
                :bridges="edgeBridges ?? []"
                :current="statusInfo?.cloud?.bridge_id"
                @refresh="refreshEdgeBridges"
              />
              <RemotesSection v-if="active === 'remotes'" />
              <NativeConnectSection v-if="active === 'nativeconnect'" />
            </div>
          </Transition>

          <!-- Shared datalist for the free-form model inputs -->
          <datalist id="woca-model-suggestions">
            <option v-for="m in modelSuggestions" :key="m" :value="m" />
          </datalist>
        </div>
      </div>

      <!-- Sticky footer: server-settings save actions -->
      <div class="settings-footer">
        <span v-if="server.serverError" class="row-hint" style="color: var(--danger)">
          {{ server.serverError }}
        </span>
        <span v-else-if="server.savedMessage" class="row-hint">{{ server.savedMessage }}</span>
        <span v-else class="row-hint">{{ t('settings.footer.hint') }}</span>
        <span v-if="server.restartRequired" class="row-hint" style="color: var(--warn)">
          {{ t('settings.footer.restart') }}
        </span>
        <button class="btn" :disabled="server.saving" @click="server.saveServer()">
          {{ server.saving ? t('settings.shared.saving') : t('settings.footer.save') }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.settings-card {
  width: min(880px, 94vw);
  height: min(640px, 86vh);
  max-width: 100%;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

/* Two-pane settings layout: left section nav, right scrollable fields. */
.settings-layout {
  display: flex;
  gap: 0;
  padding: 0;
  min-height: 0;
  flex: 1;
  overflow: hidden;
}

.settings-nav {
  flex: 0 0 168px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 14px 10px;
  border-right: 1px solid var(--border);
}

/* Identity + logout pinned to the sidebar's bottom-left. */
.settings-nav-footer {
  margin-top: auto;
  padding-top: 10px;
  border-top: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.nav-identity {
  display: flex;
  align-items: baseline;
  gap: 6px;
  padding: 4px 10px 6px;
  overflow: hidden;
}
.nav-identity-name {
  font-size: 12px;
  color: var(--text-2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.nav-identity-role {
  flex: 0 0 auto;
  font-size: 10.5px;
  padding: 0 6px;
  border-radius: 999px;
  border: 1px solid var(--border);
  color: var(--faint);
}
.nav-logout {
  color: var(--text-2);
}
.nav-logout:hover {
  color: var(--text);
}
.nav-logout:disabled {
  opacity: 0.6;
  cursor: default;
}

.settings-nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border: none;
  border-radius: var(--radius);
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  text-align: left;
  cursor: pointer;
  transition: background 0.12s ease, color 0.12s ease;
}
.settings-nav-item:hover {
  background: var(--surface-2);
  color: var(--text);
}
.settings-nav-item.active {
  background: var(--surface-2);
  color: var(--text);
  font-weight: 600;
  box-shadow: inset 2px 0 0 var(--focus);
}

.settings-content {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: 6px 24px 16px;
}

/* Managed-domains banner: lock + who manages it + where to look. */
.managed-banner {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 10px 0 6px;
  padding: 8px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface-2);
  font-size: 12px;
  color: var(--text-2);
}
.managed-banner-lock {
  flex: none;
  color: var(--faint);
}
.managed-banner-text {
  flex: 1;
  min-width: 0;
}
.managed-banner-link {
  flex: none;
  border: none;
  background: none;
  padding: 0;
  color: var(--link);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.managed-banner-link:hover {
  text-decoration: underline;
}

/* Sticky save bar shared by the server-backed sections. */
.settings-footer {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  padding: 12px 20px;
  border-top: 1px solid var(--border);
  background: var(--surface);
}
</style>
