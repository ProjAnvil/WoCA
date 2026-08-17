<script setup lang="ts">
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()
</script>

<template>
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
</template>
