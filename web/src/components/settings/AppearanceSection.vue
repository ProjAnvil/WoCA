<script setup lang="ts">
import { Moon, Sun } from 'lucide-vue-next'
import { useSettingsStore } from '../../stores/settings'

const settings = useSettingsStore()
</script>

<template>
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
</template>

<style scoped>
/* Theme choice cards */
.theme-choice {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}

.theme-option {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface);
  color: var(--text-2);
  transition: border-color 0.12s ease, background 0.12s ease;
}
.theme-option:hover {
  background: var(--surface-2);
}
.theme-option.active {
  border-color: var(--focus);
  background: var(--surface-2);
  color: var(--text);
}
</style>
