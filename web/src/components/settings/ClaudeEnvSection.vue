<script lang="ts">
import { reactive } from 'vue'
import type { ClaudeEnvState } from '../../types'

// The section component itself is v-if'd per tab switch, but its fetched
// state must survive switching away and back while the panel is open (the
// `loaded` gate below avoids refetching). Module scope gives it exactly the
// lifetime this state had inside the settings panel's setup scope before
// the split; SettingsPanel calls resetClaudeEnv() when the panel closes.
const sharedClaudeEnv = reactive({
  loaded: false,
  loading: false,
  saving: false,
  error: '',
  message: '',
  state: null as ClaudeEnvState | null,
})

export function resetClaudeEnv() {
  sharedClaudeEnv.loaded = false
  sharedClaudeEnv.loading = false
  sharedClaudeEnv.saving = false
  sharedClaudeEnv.error = ''
  sharedClaudeEnv.message = ''
  sharedClaudeEnv.state = null
}
</script>

<script setup lang="ts">
import { onMounted } from 'vue'
import { Plug } from 'lucide-vue-next'
import { getClaudeEnv, putClaudeEnv } from '../../api/client'

const claudeEnv = sharedClaudeEnv

async function loadClaudeEnv() {
  if (claudeEnv.loading) return
  claudeEnv.loading = true
  claudeEnv.error = ''
  try {
    claudeEnv.state = await getClaudeEnv()
    claudeEnv.loaded = true
  } catch (e) {
    claudeEnv.error = e instanceof Error ? e.message : String(e)
  } finally {
    claudeEnv.loading = false
  }
}

// Load on first activation of the section (mount), matching the panel's old
// `active === 'claudeenv' && !claudeEnv.loaded` watch.
onMounted(() => {
  if (!claudeEnv.loaded) void loadClaudeEnv()
})

function toggleSkill(name: string) {
  const s = claudeEnv.state?.selected
  if (!s) return
  s.skills = s.skills.includes(name) ? s.skills.filter((x) => x !== name) : [...s.skills, name]
}

function toggleMCP(name: string) {
  const s = claudeEnv.state?.selected
  if (!s) return
  s.mcp = s.mcp.includes(name) ? s.mcp.filter((x) => x !== name) : [...s.mcp, name]
}

async function saveClaudeEnv() {
  if (!claudeEnv.state) return
  claudeEnv.saving = true
  claudeEnv.error = ''
  claudeEnv.message = ''
  try {
    await putClaudeEnv({
      skills: claudeEnv.state.selected.skills,
      mcp: claudeEnv.state.selected.mcp,
    })
    claudeEnv.message = '已保存：选中的技能软链到 ~/.woca/skills 并立即生效；MCP 服务器将在下次启动时连接'
    await loadClaudeEnv()
  } catch (e) {
    claudeEnv.error = e instanceof Error ? e.message : String(e)
  } finally {
    claudeEnv.saving = false
  }
}
</script>

<template>
  <!-- Claude environment: ~/.claude skills + global MCP servers,
       checkbox-selected per agent start. -->
  <section class="setting-section">
    <div class="setting-section-title">
      <Plug :size="13" style="vertical-align: -2px" /> 技能与 MCP（导入 Claude Code）
    </div>
    <p class="mem-intro">
      检测 ~/.claude/skills 与 ~/.claude.json 的全局 MCP 配置。勾选后技能软链到
      ~/.woca/skills（立即生效），MCP 服务器写入 ~/.woca/mcp.json 并在 agent
      启动时连接。
    </p>

    <div v-if="claudeEnv.loading" class="row-hint">检测中…</div>
    <div v-else-if="claudeEnv.error" class="row-hint" style="color: var(--danger, #e5484d)">
      {{ claudeEnv.error }}
    </div>

    <template v-if="claudeEnv.state">
      <div class="setting-subgroup">
        <div class="row-label">Skill（{{ claudeEnv.state.selected.skills.length }}/{{ claudeEnv.state.skills.length }}）</div>
        <div class="row-hint">勾选的技能通过 / 命令与补全列表提供给 agent</div>
        <div v-if="!claudeEnv.state.skills.length" class="row-hint">
          未在 ~/.claude/skills 检测到技能
        </div>
        <div v-else class="env-check-list">
          <label v-for="s in claudeEnv.state.skills" :key="s.name" class="env-check">
            <input
              type="checkbox"
              :checked="claudeEnv.state.selected.skills.includes(s.name)"
              @change="toggleSkill(s.name)"
            />
            <span class="env-check-name">{{ s.name }}</span>
            <span class="env-check-desc" :title="s.description">{{ s.description }}</span>
          </label>
        </div>
      </div>

      <div class="setting-subgroup">
        <div class="row-label">MCP（{{ claudeEnv.state.selected.mcp.length }}/{{ claudeEnv.state.mcp.length }}）</div>
        <div class="row-hint">勾选的服务器在 agent 启动时作为工具连接（下次启动生效）</div>
        <div v-if="!claudeEnv.state.mcp.length" class="row-hint">
          未在 ~/.claude.json 检测到全局 MCP 服务器
        </div>
        <div v-else class="env-check-list">
          <label v-for="m in claudeEnv.state.mcp" :key="m.name" class="env-check">
            <input
              type="checkbox"
              :checked="claudeEnv.state.selected.mcp.includes(m.name)"
              @change="toggleMCP(m.name)"
            />
            <span class="env-check-name">{{ m.name }}</span>
            <span class="env-check-desc" :title="m.url || m.command">
              {{ m.type }} · {{ m.url || m.command }}{{ m.has_env ? ' · 含环境变量' : '' }}
            </span>
          </label>
        </div>
      </div>

      <div class="setting-row">
        <button class="btn" :disabled="claudeEnv.saving" @click="saveClaudeEnv()">
          {{ claudeEnv.saving ? '保存中…' : '保存选择' }}
        </button>
        <span v-if="claudeEnv.message" class="row-hint" style="margin-left: 10px">{{ claudeEnv.message }}</span>
      </div>
    </template>
  </section>
</template>

<style scoped>
/* 技能与 MCP 勾选列表 */
.env-check-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 6px;
  max-height: 320px;
  overflow-y: auto;
}
.env-check {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius);
  cursor: pointer;
}
.env-check:hover {
  background: var(--surface-hover);
}
.env-check input[type='checkbox'] {
  flex: 0 0 auto;
  transform: translateY(1px);
  accent-color: var(--focus);
}
.env-check-desc {
  flex: 1 1 auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  color: var(--faint);
}
</style>
