<script setup lang="ts">
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()
</script>

<template>
  <!-- Model (session preferences) -->
  <section class="setting-section">
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
</template>
