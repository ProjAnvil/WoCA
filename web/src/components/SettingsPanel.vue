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
  LogOut,
  Network,
  Palette,
  Plug,
  Puzzle,
  Radio,
  Server,
  Settings2,
  X,
} from 'lucide-vue-next'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/settings'
import { logout as apiLogout } from '../api/client'
import AppearanceSection from './settings/AppearanceSection.vue'
import PreferencesSection from './settings/PreferencesSection.vue'
import ProviderSection from './settings/ProviderSection.vue'
import ClaudeEnvSection, { resetClaudeEnv } from './settings/ClaudeEnvSection.vue'
import PluginsSection, { resetPluginState } from './settings/PluginsSection.vue'
import StorageSection from './settings/StorageSection.vue'
import AdminSection from './settings/AdminSection.vue'
import CloudAccessSection from './settings/CloudAccessSection.vue'
import EdgeSwitchSection from './settings/EdgeSwitchSection.vue'
import ManagedSection, { resetManagedState } from './settings/ManagedSection.vue'
import { getStatus, getEdgeBridges, type BridgeStatus, type EdgeBridge } from '../api/client'
import MessagePlaneSection from './settings/MessagePlaneSection.vue'
import MemorySection from './settings/MemorySection.vue'
import ModelSection from './settings/ModelSection.vue'
import StatsSection from './settings/StatsSection.vue'

const { t } = useI18n()
const settings = useSettingsStore()
const auth = useAuthStore()

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
  ...(settings.server?.msg_plane_available
    ? [{ id: 'msgplane', label: 'settings.nav.msgplane', icon: Radio }]
    : []),
  ...baseSections.slice(7), // memory, model, stats
  // The bridge switcher (接入点) appears only behind an edge access
  // point — the probe answers null on direct-bridge and personal
  // origins, so nothing else changes shape.
  ...(edgeBridges.value?.length
    ? [{ id: 'edge', label: 'settings.nav.edge', icon: Network }]
    : []),
  // The bridge cloud-access page (edge URL / enrollment, §6.5) and the
  // read-only admin page (sessions/audit — v1.7: no key management on
  // the bridge) — admin keys only.
  ...(auth.me?.role === 'admin' || statusAuth.value === 'local'
    ? [{ id: 'cloudaccess', label: 'settings.nav.cloudaccess', icon: CloudUpload }]
    : []),
  ...(auth.me?.role === 'admin' ? [{ id: 'admin', label: 'settings.nav.admin', icon: ShieldCheck }] : []),
])

const active = ref<string>('appearance')

// The bridge edition's model-provider domain is cloud-governed (the catalog
// pull + verify is the selection surface; keys never live user-side), and
// on apikey bridges only admins hold settings:write — so members would face
// editable fields that can only fail on save. Gray the section instead.
const providerLocked = computed(
  () =>
    statusInfo.value?.edition === 'bridge' &&
    (statusInfo.value.managed || Boolean(statusInfo.value.cloud?.enrolled)) &&
    statusAuth.value !== 'local',
)
const isBridge = computed(() => statusInfo.value?.edition === 'bridge')

// Logout rides the same-origin auth route: the bridge revokes at the core,
// the edge intercepts its own — the reload lands on the login gate either
// way. Only deployments with sessions (apikey posture) show it.
const canLogout = computed(() => Boolean(auth.me) || statusAuth.value === 'apikey')
async function doLogout() {
  try {
    await apiLogout()
  } catch {
    // A dead channel still logged out locally; the reload decides the gate.
  }
  auth.me = null
  window.location.reload()
}

// The cloud-access section follows the auth posture (§6.6): admin keys on
// apikey bridges, and every local-trust bridge — the embedded default is
// configured from the wails app by its owner.
const statusAuth = ref<string>('')
const statusInfo = ref<BridgeStatus | null>(null)
// Edge inventory probe: non-null only when this origin is an edge access
// point (the bridge-switch section's gate).
const edgeBridges = ref<EdgeBridge[] | null>(null)
onMounted(async () => {
  try {
    const st = await getStatus()
    statusAuth.value = st.auth ?? ''
    statusInfo.value = st
  } catch {
    statusAuth.value = ''
    statusInfo.value = null
  }
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

// The claudeenv / plugins sections keep their fetch state at module scope so
// it survives section switches; closing the panel must reset it, matching the
// lifetime that state had when it lived in this component's setup scope.
onBeforeUnmount(() => {
  resetClaudeEnv()
  resetPluginState()
  resetManagedState()
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
               the embedded personal edition has no session to end. -->
          <div v-if="canLogout" class="settings-nav-footer">
            <div class="nav-identity">
              <span class="nav-identity-name">{{ auth.me?.remark || auth.me?.user_id }}</span>
              <span v-if="auth.me?.role" class="nav-identity-role">{{ auth.me.role }}</span>
            </div>
            <button class="settings-nav-item nav-logout" @click="doLogout()">
              <LogOut :size="14" />
              <span>{{ t('settings.logout') }}</span>
            </button>
          </div>
        </nav>

        <!-- Right pane: the selected section's fields -->
        <div class="settings-content">
          <div v-if="settings.managedFields.length" class="managed-banner">
            {{ t('settings.managedBanner') }}
          </div>
          <AppearanceSection v-if="active === 'appearance'" />
          <PreferencesSection v-if="active === 'preferences'" />
          <ProviderSection v-if="active === 'provider'" :locked="providerLocked" />
          <ClaudeEnvSection v-if="active === 'claudeenv'" :bridge="isBridge" />
          <ManagedSection v-if="active === 'managed'" :status="statusInfo" />
          <PluginsSection v-if="active === 'plugins'" :bridge="isBridge" />
          <StorageSection v-if="active === 'storage'" :bridge="isBridge" />
          <!-- Message plane is not a private-cloud concept (product design
               §2 rule 2): the section disappears on managed bridges. -->
          <MessagePlaneSection
            v-if="active === 'msgplane' && settings.server?.msg_plane_available && !settings.managedFields.length"
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

          <!-- Shared datalist for the free-form model inputs -->
          <datalist id="woca-model-suggestions">
            <option v-for="m in modelSuggestions" :key="m" :value="m" />
          </datalist>
        </div>
      </div>

      <!-- Sticky footer: server-settings save actions -->
      <div class="settings-footer">
        <span v-if="settings.serverError" class="row-hint" style="color: var(--danger, #e5484d)">
          {{ settings.serverError }}
        </span>
        <span v-else-if="settings.savedMessage" class="row-hint">{{ settings.savedMessage }}</span>
        <span v-else class="row-hint">{{ t('settings.footer.hint') }}</span>
        <span v-if="settings.restartRequired" class="row-hint" style="color: var(--warn, #f5a524)">
          {{ t('settings.footer.restart') }}
        </span>
        <button class="btn" :disabled="settings.saving" @click="settings.saveServer()">
          {{ settings.saving ? t('settings.shared.saving') : t('settings.footer.save') }}
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
  color: var(--danger, #e5484d);
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
