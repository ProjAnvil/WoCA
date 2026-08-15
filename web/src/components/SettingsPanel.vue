<script setup lang="ts">
import { ref } from 'vue'
import { Brain, Cpu, Database, Moon, Palette, Server, Settings2, Sun, X } from 'lucide-vue-next'
import { useSettingsStore } from '../stores/settings'

const settings = useSettingsStore()

// Sidebar navigation: each entry maps to one section of the right pane.
const sections = [
  { id: 'appearance', label: '外观', icon: Palette },
  { id: 'preferences', label: '偏好', icon: Settings2 },
  { id: 'provider', label: '模型服务商', icon: Server },
  { id: 'storage', label: '存储', icon: Database },
  { id: 'memory', label: '记忆', icon: Brain },
  { id: 'model', label: '模型', icon: Cpu },
]

const active = ref<string>('appearance')

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
]
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
            <span>{{ s.label }}</span>
          </button>
        </nav>

        <!-- Right pane: the selected section's fields -->
        <div class="settings-content">
          <!-- Appearance -->
          <section v-if="active === 'appearance'" class="setting-section">
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
          <section v-if="active === 'preferences'" class="setting-section">
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

            <div class="setting-row">
              <div>
                <div class="row-label">调试日志</div>
                <div class="row-hint">
                  开启后记录 graph / session / message 级完整日志（即时生效）{{
                    settings.logDir ? `· ${settings.logDir}` : ''
                  }}
                </div>
              </div>
              <button
                class="switch"
                :class="{ on: settings.debugLog }"
                title="服务端持久化设置：开启后气泡下出现 debug 复制按钮"
                @click="settings.toggleDebugLog()"
              ></button>
            </div>
          </section>

          <!-- Provider (persisted server-side; keys encrypted at rest).
               One Base URL + one API Key field target the selected provider. -->
          <section v-if="active === 'provider'" class="setting-section">
            <div class="setting-section-title"><Server :size="13" style="vertical-align: -2px" /> 模型服务商</div>

            <div class="setting-row">
              <div>
                <div class="row-label">服务商</div>
                <div class="row-hint">Base URL 与 API Key 只需填写一组，作用于所选服务商</div>
              </div>
              <select v-model="settings.provider" class="field" style="max-width: 200px">
                <option value="anthropic">anthropic</option>
                <option value="openai">openai</option>
              </select>
            </div>

            <div class="setting-row">
              <div>
                <div class="row-label">Base URL</div>
                <div class="row-hint">留空使用官方默认（当前：{{ settings.provider }}）</div>
              </div>
              <input
                v-model="settings.baseURL"
                class="field"
                style="max-width: 260px"
                :placeholder="settings.provider === 'anthropic' ? 'https://api.anthropic.com' : 'https://api.openai.com/v1'"
              />
            </div>

            <div class="setting-row">
              <div>
                <div class="row-label">API Key</div>
                <div class="row-hint">
                  {{ settings.apiKeySet ? '已设置（加密存储）' : '未设置' }}；输入新值以更新（当前：{{ settings.provider }}）
                </div>
              </div>
              <input
                v-model="settings.apiKey"
                class="field"
                style="max-width: 260px"
                type="password"
                autocomplete="off"
                :placeholder="settings.provider === 'anthropic' ? 'sk-ant-…' : 'sk-…'"
              />
            </div>

            <div class="setting-row">
              <div>
                <div class="row-label">默认模型</div>
                <div class="row-hint">可自由输入任意模型名（支持自定义/中转模型）</div>
              </div>
              <input
                v-model="settings.serverModel"
                class="field"
                style="max-width: 260px"
                list="woca-model-suggestions"
                placeholder="claude-sonnet-4-5"
              />
            </div>
          </section>

          <!-- Storage (checkpoint + memoryStore backends) -->
          <section v-if="active === 'storage'" class="setting-section">
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
              <input
                v-model="settings.postgresURL"
                class="field"
                style="max-width: 260px"
                type="password"
                autocomplete="off"
                placeholder="postgres://user:pass@host/db"
              />
            </div>

            <div v-if="settings.server" class="setting-row">
              <div>
                <div class="row-label">数据库位置</div>
                <div class="row-hint">{{ settings.server.database }}</div>
              </div>
            </div>
          </section>

          <!-- Memory (ladyM factual memory engine) -->
          <section v-if="active === 'memory'" class="setting-section">
            <div class="setting-section-title"><Brain :size="13" style="vertical-align: -2px" /> 记忆</div>
            <p class="mem-intro">woca 会记住这个工作区的决策、偏好与代码知识——越用越懂你的项目。</p>

            <div class="setting-row">
              <div>
                <div class="row-label">记忆引擎</div>
                <div class="row-hint">关闭后不再读写记忆（实时生效）</div>
              </div>
              <select v-model="settings.memoryEngine" class="field" style="max-width: 200px">
                <option value="ladym">启用（默认）</option>
                <option value="none">关闭</option>
              </select>
            </div>

            <template v-if="settings.memoryEngine === 'ladym'">
              <div class="setting-row">
                <div>
                  <div class="row-label">自动记忆</div>
                  <div class="row-hint">对话结束后自动沉淀值得记住的决策与偏好</div>
                </div>
                <button
                  class="switch"
                  :class="{ on: settings.ladymAutoRetain }"
                  @click="settings.ladymAutoRetain = !settings.ladymAutoRetain"
                ></button>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">后台整理</div>
                  <div class="row-hint">定期把经历提炼为事实与洞察、预判下一步（消耗少量模型调用）</div>
                </div>
                <button
                  class="switch"
                  :class="{ on: settings.ladymSystem2 }"
                  @click="settings.ladymSystem2 = !settings.ladymSystem2"
                ></button>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">召回条数</div>
                  <div class="row-hint">每轮对话注入的相关记忆条数（1–20）</div>
                </div>
                <input
                  v-model.number="settings.ladymTopK"
                  class="field"
                  style="max-width: 100px"
                  type="number"
                  min="1"
                  max="20"
                />
              </div>

              <div class="setting-subgroup">
                <div class="row-label">模型</div>
                <div class="row-hint">默认继承上方模型配置；也可单独指定（如用便宜的 embedding 模型）</div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">Embedding</div>
                    <div class="row-hint">向量检索使用的 embedding 提供方</div>
                  </div>
                  <select v-model="settings.ladymEmbedding" class="field" style="max-width: 200px">
                    <option value="inherit">继承 woca</option>
                    <option value="hashing">hashing（离线）</option>
                    <option value="openai">openai</option>
                    <option value="ollama">ollama</option>
                    <option value="http">http</option>
                  </select>
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">Embedding 模型</div>
                    <div class="row-hint">留空使用所选提供方的默认模型</div>
                  </div>
                  <input
                    v-model="settings.ladymEmbeddingModel"
                    class="field"
                    style="max-width: 260px"
                    placeholder="text-embedding-3-small"
                  />
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">Embedding Base URL</div>
                    <div class="row-hint">openai / ollama / http 提供方的服务地址</div>
                  </div>
                  <input
                    v-model="settings.ladymEmbeddingBaseURL"
                    class="field"
                    style="max-width: 260px"
                    placeholder="继承 OpenAI Base URL"
                  />
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">Embedding API Key</div>
                    <div class="row-hint">
                      {{ settings.ladymEmbeddingKeySet ? '已设置（加密存储）' : '未设置（继承 OpenAI Key）' }}
                    </div>
                  </div>
                  <input
                    v-model="settings.ladymEmbeddingAPIKey"
                    class="field"
                    style="max-width: 260px"
                    type="password"
                    autocomplete="off"
                    placeholder="输入新值以更新"
                  />
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">整理模型（LLM）</div>
                    <div class="row-hint">继承 = 用当前模型做离线整理；none = 纯本地启发式（洞察/预判停用）</div>
                  </div>
                  <select v-model="settings.ladymLLM" class="field" style="max-width: 200px">
                    <option value="inherit">继承 woca</option>
                    <option value="none">none</option>
                  </select>
                </div>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">数据库路径</div>
                  <div class="row-hint">留空使用 ~/.woca/ladym.db（实时生效）</div>
                </div>
                <input
                  v-model="settings.ladymDBPath"
                  class="field"
                  style="max-width: 260px"
                  placeholder="~/.woca/ladym.db"
                />
              </div>
            </template>
          </section>

          <!-- Model (session preferences) -->
          <section v-if="active === 'model'" class="setting-section">
            <div class="setting-section-title">模型</div>

            <div class="setting-row">
              <div>
                <div class="row-label">会话默认模型</div>
                <div class="row-hint">可自由输入任意模型名（支持自定义/中转模型）</div>
              </div>
              <input
                v-model="settings.model"
                class="field"
                style="max-width: 260px"
                list="woca-model-suggestions"
                placeholder="claude-sonnet-4-5"
              />
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
        <span v-else class="row-hint">服务商/存储设置保存到数据库，密钥加密存储</span>
        <span v-if="settings.restartRequired" class="row-hint" style="color: var(--warn, #f5a524)">
          后端变更将在重启 woca 后生效
        </span>
        <button class="btn" :disabled="settings.saving" @click="settings.saveServer()">
          {{ settings.saving ? '保存中…' : '保存到数据库' }}
        </button>
      </div>
    </div>
  </div>
</template>
