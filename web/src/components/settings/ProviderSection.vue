<script setup lang="ts">
import { computed } from 'vue'
import { Server } from 'lucide-vue-next'
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()

// Per-provider defaults shown as input placeholders.
const providerDefaults: Record<string, { base: string; key: string }> = {
  anthropic: { base: 'https://api.anthropic.com', key: 'sk-ant-…' },
  openai: { base: 'https://api.openai.com/v1', key: 'sk-…' },
  glm: { base: 'https://open.bigmodel.cn/api/anthropic（默认）', key: '智谱 API Key' },
  deepseek: { base: 'https://api.deepseek.com/anthropic（默认）', key: 'deepseek-…' },
}
const providerHint = computed(() => providerDefaults[settings.provider] ?? providerDefaults.anthropic)
</script>

<template>
  <!-- Provider (persisted server-side; keys encrypted at rest).
       One Base URL + one API Key field target the selected provider. -->
  <section class="setting-section">
    <div class="setting-section-title"><Server :size="13" style="vertical-align: -2px" /> 模型服务商</div>

    <div class="setting-row">
      <div>
        <div class="row-label">服务商</div>
        <div class="row-hint">Base URL 与 API Key 只需填写一组，作用于所选服务商</div>
      </div>
      <select v-model="settings.provider" class="field" style="max-width: 200px">
        <option value="anthropic">anthropic</option>
        <option value="openai">openai</option>
        <option value="glm">glm（智谱）</option>
        <option value="deepseek">deepseek</option>
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
        :placeholder="providerHint.base"
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
        :placeholder="providerHint.key"
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
</template>
