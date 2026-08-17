<script setup lang="ts">
import { Database } from 'lucide-vue-next'
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()
</script>

<template>
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
</template>
