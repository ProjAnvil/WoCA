<script setup lang="ts">
import { Languages, Monitor, Moon, Sun } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
</script>

<template>
  <!-- Appearance -->
  <section class="setting-section">
    <div class="setting-section-title">{{ t('settings.appearance.title') }}</div>
    <div class="theme-choice">
      <button
        class="theme-option"
        :class="{ active: !settings.isDark }"
        @click="settings.isDark = false"
      >
        <Sun :size="16" />
        <span>{{ t('settings.appearance.light') }}</span>
      </button>
      <button
        class="theme-option"
        :class="{ active: settings.isDark }"
        @click="settings.isDark = true"
      >
        <Moon :size="16" />
        <span>{{ t('settings.appearance.dark') }}</span>
      </button>
    </div>
    <div class="setting-section-title lang-title">{{ t('settings.appearance.language') }}</div>
    <div class="theme-choice lang-choice">
      <button
        class="theme-option"
        :class="{ active: settings.localePref === 'auto' }"
        @click="settings.setLocale('auto')"
      >
        <Monitor :size="16" />
        <span>{{ t('settings.appearance.langAuto') }}</span>
      </button>
      <button
        class="theme-option"
        :class="{ active: settings.localePref === 'zh-CN' }"
        @click="settings.setLocale('zh-CN')"
      >
        <Languages :size="16" />
        <span>简体中文</span>
      </button>
      <button
        class="theme-option"
        :class="{ active: settings.localePref === 'en' }"
        @click="settings.setLocale('en')"
      >
        <Languages :size="16" />
        <span>English</span>
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
.lang-choice {
  grid-template-columns: 1fr 1fr 1fr;
}
.lang-title {
  margin-top: 16px;
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
