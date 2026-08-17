<script lang="ts">
import { reactive } from 'vue'
import type { PluginStatus } from '../../types'

// The section component itself is v-if'd per tab switch, but the plugin list
// and status messages must survive switching away and back while the panel is
// open (otherwise every revisit would flash the loading placeholder). Module
// scope gives it exactly the lifetime this state had inside the settings
// panel's setup scope before the split; SettingsPanel calls resetPluginState()
// when the panel closes.
const sharedPluginState = reactive({
  loaded: false,
  loading: false,
  error: '',
  message: '',
  plugins: [] as PluginStatus[],
  toggling: '',
})

let pluginTimer: ReturnType<typeof setInterval> | null = null

export function resetPluginState() {
  if (pluginTimer != null) {
    clearInterval(pluginTimer)
    pluginTimer = null
  }
  sharedPluginState.loaded = false
  sharedPluginState.loading = false
  sharedPluginState.error = ''
  sharedPluginState.message = ''
  sharedPluginState.plugins = []
  sharedPluginState.toggling = ''
}
</script>

<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import { Puzzle } from 'lucide-vue-next'
import { getPlugins, installPlugin, putPlugins } from '../../api/client'

const pluginState = sharedPluginState

async function loadPlugins() {
  if (pluginState.loading) return
  pluginState.loading = true
  pluginState.error = ''
  try {
    const res = await getPlugins()
    pluginState.plugins = res.plugins
    pluginState.loaded = true
    scheduleInstallPolling()
  } catch (e) {
    pluginState.error = e instanceof Error ? e.message : String(e)
  } finally {
    pluginState.loading = false
  }
}

// While any runtime is installing, poll for progress; stop when idle.
function scheduleInstallPolling() {
  const installing = pluginState.plugins.some((p) => p.state === 'installing')
  if (installing && pluginTimer == null) {
    pluginTimer = setInterval(() => void loadPlugins(), 2000)
  } else if (!installing && pluginTimer != null) {
    clearInterval(pluginTimer)
    pluginTimer = null
  }
}

// Entering the section always refreshes (not just the first visit):
// unmounting cleared the install-poll timer, and loadPlugins' internal
// `loaded` gate would leave "installing" stuck at its stale state.
// scheduleInstallPolling covers the case where a fetch is already in
// flight and the guard skips the refresh — the stale state still says
// installing, so polling must resume right away.
onMounted(() => {
  void loadPlugins()
  scheduleInstallPolling()
})

// Leaving the section (or closing the panel) stops the polling.
onBeforeUnmount(() => {
  if (pluginTimer != null) clearInterval(pluginTimer)
})

async function togglePlugin(p: PluginStatus) {
  if (pluginState.toggling) return
  const enable = !p.enabled
  pluginState.toggling = p.name
  pluginState.error = ''
  pluginState.message = ''
  const names = pluginState.plugins
    .filter((x) => (x.name === p.name ? enable : x.enabled))
    .map((x) => x.name)
  try {
    await putPlugins(names)
    p.enabled = enable
    pluginState.message = enable
      ? `已启用 ${p.name}${p.state !== 'ready' ? '（运行时未安装，点击「安装」后工具才可用）' : '，工具立即生效'}`
      : `已停用 ${p.name}，工具立即移除`
  } catch (e) {
    pluginState.error = e instanceof Error ? e.message : String(e)
  } finally {
    pluginState.toggling = ''
  }
}

async function installOne(p: PluginStatus, reinstall = false) {
  pluginState.error = ''
  pluginState.message = ''
  try {
    await installPlugin(p.name, reinstall)
    p.state = 'installing'
    p.log = []
    pluginState.message = reinstall
      ? `${p.name} 开始升级（删除旧环境后按当前固定版本重装，共享运行时保留）`
      : `${p.name} 开始安装（下载运行时与依赖，可能需要几分钟）`
    scheduleInstallPolling()
  } catch (e) {
    pluginState.error = e instanceof Error ? e.message : String(e)
  }
}

const kindLabels: Record<string, string> = {
  golang: 'Go 内置',
  python: 'Python · uv',
}

const stateLabels: Record<string, string> = {
  not_installed: '未安装',
  installing: '安装中…',
  ready: '已就绪',
  error: '安装失败',
}
</script>

<template>
  <!-- Built-in capability plugins: enable toggles + runtime installs -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Puzzle :size="13" style="vertical-align: -2px" /> 插件
    </div>
    <p class="mem-intro">
      内置能力插件随 woca 二进制分发，运行时（pinchtab / uv+Python）按需下载到
      ~/.woca/plugins 并校验 SHA-256。启用后工具立即注入 agent。
    </p>

    <div v-if="pluginState.loading && !pluginState.loaded" class="row-hint">加载中…</div>
    <div v-else-if="pluginState.error" class="row-hint" style="color: var(--danger, #e5484d)">
      {{ pluginState.error }}
    </div>
    <div v-if="pluginState.message" class="row-hint" style="margin-bottom: 6px">
      {{ pluginState.message }}
    </div>

    <div v-for="p in pluginState.plugins" :key="p.name" class="plugin-card">
      <div class="plugin-card-head">
        <div>
          <span class="env-check-name">{{ p.name }}</span>
          <span class="plugin-badge">{{ kindLabels[p.kind] ?? p.kind }}</span>
          <span v-if="p.version" class="plugin-badge plugin-badge-dim">{{ p.version }}</span>
          <span class="plugin-state" :data-state="p.state">
            {{ stateLabels[p.state] ?? p.state }}
          </span>
        </div>
        <button
          class="switch"
          :class="{ on: p.enabled }"
          :disabled="pluginState.toggling === p.name"
          :title="p.enabled ? '点击停用（工具立即移除）' : '点击启用'"
          @click="togglePlugin(p)"
        ></button>
      </div>
      <div class="row-hint">{{ p.description }}</div>
      <div class="row-hint">
        工具：<code v-for="t in p.tools" :key="t" class="plugin-tool">{{ t }}</code>
      </div>
      <div v-if="p.state === 'ready' && p.detail" class="row-hint plugin-versions">
        {{ p.detail }}
      </div>
      <div v-if="p.state === 'error' && p.detail" class="row-hint plugin-error">
        {{ p.detail }}
      </div>
      <div v-if="p.log?.length" class="plugin-log">
        <div v-for="(line, i) in p.log.slice(-8)" :key="i">{{ line }}</div>
      </div>
      <div class="plugin-actions">
        <button
          v-if="p.state === 'ready' && p.runtime"
          class="btn"
          title="删除环境后按当前固定版本重装（升级 / 修复）；共享运行时与缓存保留"
          @click="installOne(p, true)"
        >
          升级 / 重装
        </button>
        <button
          v-if="p.state !== 'ready' && p.runtime"
          class="btn"
          :disabled="p.state === 'installing'"
          @click="installOne(p)"
        >
          {{ p.state === 'installing' ? '安装中…' : '下载并安装' }}
        </button>
      </div>
    </div>
  </section>
</template>

<style scoped>
/* ---------------- plugin cards ---------------- */

.plugin-card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 10px 12px;
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.plugin-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.plugin-badge {
  display: inline-block;
  margin-left: 8px;
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--border);
  font-size: 11px;
  color: var(--faint);
  vertical-align: 1px;
}
.plugin-badge-dim {
  color: var(--faint);
}
.plugin-state {
  margin-left: 8px;
  font-size: 11px;
  vertical-align: 1px;
  color: var(--faint);
}
.plugin-state[data-state='ready'] {
  color: #46a758;
}
.plugin-state[data-state='installing'] {
  color: var(--warn, #f5a524);
}
.plugin-state[data-state='error'] {
  color: var(--danger);
}
.plugin-tool {
  display: inline-block;
  margin: 1px 4px 1px 0;
  padding: 1px 6px;
  border-radius: var(--radius);
  background: var(--surface-hover);
  font-size: 11px;
}
.plugin-error {
  color: var(--danger);
  white-space: pre-wrap;
  word-break: break-all;
}
.plugin-log {
  max-height: 140px;
  overflow-y: auto;
  border-radius: var(--radius);
  background: var(--surface-hover);
  padding: 6px 8px;
  font-size: 11px;
  font-family: var(--mono, ui-monospace, monospace);
  color: var(--faint);
  white-space: pre-wrap;
  word-break: break-all;
}
.plugin-actions {
  display: flex;
  gap: 8px;
}
.plugin-versions {
  color: #46a758;
}
</style>
