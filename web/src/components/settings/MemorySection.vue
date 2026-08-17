<script setup lang="ts">
import { Brain } from 'lucide-vue-next'
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()
</script>

<template>
  <!-- Memory (ladyM factual memory engine) -->
  <section class="setting-section">
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
          <div class="row-label">整理间隔（秒）</div>
          <div class="row-hint">开启后台整理后，同一工作区两次整理的最小间隔（≥60，默认 300）</div>
        </div>
        <input
          v-model.number="settings.ladymSystem2Interval"
          class="field"
          style="max-width: 100px"
          type="number"
          min="60"
          step="30"
        />
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
</template>
