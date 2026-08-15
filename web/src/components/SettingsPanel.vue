<script setup lang="ts">
import { Database, Moon, Server, Sun, X } from 'lucide-vue-next'
import { useSettingsStore } from '../stores/settings'

const settings = useSettingsStore()
</script>

<template>
  <div class="overlay" @click.self="settings.closePanel()">
    <div class="card settings-card">
      <div class="card-head">
        <span class="card-title">设置</span>
        <button class="icon-btn" @click="settings.closePanel()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body">
        <!-- Appearance -->
        <section class="setting-section">
          <div class="setting-section-title">外观</div>
          <div class="theme-choice">
            <button
              class="theme-option"
              :class="{ active: !settings.isDark }"
              @click="settings.isDark = false"
            >
              <Sun :size="16" />
              <span>浅色</span>
            </button>
            <button
              class="theme-option"
              :class="{ active: settings.isDark }"
              @click="settings.isDark = true"
            >
              <Moon :size="16" />
              <span>深色</span>
            </button>
          </div>
        </section>

        <!-- Preferences -->
        <section class="setting-section">
          <div class="setting-section-title">偏好</div>

          <div class="setting-row">
            <div>
              <div class="row-label">自动折叠工具输出</div>
              <div class="row-hint">默认折叠较长的工具结果</div>
            </div>
            <button
              class="switch"
              :class="{ on: settings.autoCollapseTools }"
              @click="settings.autoCollapseTools = !settings.autoCollapseTools"
            ></button>
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">Markdown 预览</div>
              <div class="row-hint">点击工作区文件展开 Markdown 预览</div>
            </div>
            <button
              class="switch"
              :class="{ on: settings.markdownPreview }"
              @click="settings.markdownPreview = !settings.markdownPreview"
            ></button>
          </div>
        </section>

        <!-- Provider (persisted server-side; keys encrypted at rest) -->
        <section class="setting-section">
          <div class="setting-section-title"><Server :size="13" style="vertical-align: -2px" /> 模型服务商</div>

          <div class="setting-row">
            <div class="row-label">服务商</div>
            <select v-model="settings.provider" class="field" style="max-width: 200px">
              <option value="anthropic">anthropic</option>
              <option value="openai">openai</option>
            </select>
          </div>

          <div class="setting-row">
            <div class="row-label">默认模型</div>
            <input v-model="settings.serverModel" class="field" style="max-width: 200px"
              placeholder="claude-sonnet-4-5" />
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">Anthropic Base URL</div>
              <div class="row-hint">留空使用官方默认</div>
            </div>
            <input v-model="settings.anthropicBaseURL" class="field" style="max-width: 220px"
              placeholder="https://api.anthropic.com" />
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">Anthropic API Key</div>
              <div class="row-hint">
                {{ settings.server?.anthropic_api_key_set ? '已设置（加密存储）' : '未设置' }}；输入新值以更新
              </div>
            </div>
            <input v-model="settings.anthropicAPIKey" class="field" style="max-width: 220px"
              type="password" autocomplete="off" placeholder="sk-ant-…" />
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">OpenAI Base URL</div>
              <div class="row-hint">留空使用官方默认</div>
            </div>
            <input v-model="settings.openaiBaseURL" class="field" style="max-width: 220px"
              placeholder="https://api.openai.com/v1" />
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">OpenAI API Key</div>
              <div class="row-hint">
                {{ settings.server?.openai_api_key_set ? '已设置（加密存储）' : '未设置' }}；输入新值以更新
              </div>
            </div>
            <input v-model="settings.openaiAPIKey" class="field" style="max-width: 220px"
              type="password" autocomplete="off" placeholder="sk-…" />
          </div>
        </section>

        <!-- Storage (checkpoint + memoryStore backends) -->
        <section class="setting-section">
          <div class="setting-section-title">
            <Database :size="13" style="vertical-align: -2px" /> 存储
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">Checkpoint 后端</div>
              <div class="row-hint">图状态检查点持久化（中断/恢复）</div>
            </div>
            <select v-model="settings.checkpointBackend" class="field" style="max-width: 200px">
              <option value="sqlite">sqlite（默认，~/.woca）</option>
              <option value="postgres">postgres</option>
              <option value="file">file（目录）</option>
              <option value="none">none</option>
            </select>
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">MemoryStore 后端</div>
              <div class="row-hint">长期记忆键值存储</div>
            </div>
            <select v-model="settings.memoryBackend" class="field" style="max-width: 200px">
              <option value="sqlite">sqlite（默认，~/.woca）</option>
              <option value="postgres">postgres</option>
              <option value="memory">memory（内存）</option>
              <option value="none">none</option>
            </select>
          </div>

          <div class="setting-row">
            <div>
              <div class="row-label">Postgres DSN</div>
              <div class="row-hint">
                {{ settings.server?.postgres_url_set ? '已设置（加密存储）' : '未设置' }}；选择 postgres 后端时必填
              </div>
            </div>
            <input v-model="settings.postgresURL" class="field" style="max-width: 220px"
              type="password" autocomplete="off" placeholder="postgres://user:pass@host/db" />
          </div>

          <div v-if="settings.server" class="setting-row">
            <div>
              <div class="row-label">数据库位置</div>
              <div class="row-hint">{{ settings.server.database }}</div>
            </div>
          </div>
        </section>

        <!-- Model (session preferences) -->
        <section class="setting-section">
          <div class="setting-section-title">模型</div>

          <div class="setting-row">
            <div class="row-label">会话默认模型</div>
            <select v-model="settings.model" class="field" style="max-width: 200px">
              <option value="claude-sonnet-4-5">claude-sonnet-4-5</option>
              <option value="claude-haiku-4-5">claude-haiku-4-5</option>
              <option value="gpt-4o">gpt-4o</option>
            </select>
          </div>

          <div class="setting-row">
            <div class="row-label">权限模式</div>
            <select v-model="settings.permissionMode" class="field" style="max-width: 200px">
              <option value="default">default</option>
              <option value="acceptEdits">acceptEdits</option>
              <option value="bypassPermissions">bypassPermissions</option>
              <option value="readOnly">readOnly</option>
              <option value="plan">plan</option>
            </select>
          </div>
        </section>

        <!-- Server settings actions -->
        <section class="setting-section">
          <div class="setting-row" style="justify-content: flex-end; gap: 10px">
            <span v-if="settings.serverError" class="row-hint" style="color: var(--danger, #e5484d)">
              {{ settings.serverError }}
            </span>
            <span v-else-if="settings.savedMessage" class="row-hint">{{ settings.savedMessage }}</span>
            <button class="btn" :disabled="settings.saving" @click="settings.saveServer()">
              {{ settings.saving ? '保存中…' : '保存到数据库' }}
            </button>
          </div>
          <div v-if="settings.restartRequired" class="row-hint" style="text-align: right">
            后端变更将在重启 woca 后生效
          </div>
        </section>
      </div>
    </div>
  </div>
</template>
