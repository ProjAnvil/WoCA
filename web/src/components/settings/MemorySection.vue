<script setup lang="ts">
import { Brain } from 'lucide-vue-next'
import { useI18n } from 'vue-i18n'
import { useSettingsStore } from '../../stores/settings'

const { t } = useI18n()
const settings = useSettingsStore()
// The memory domain is operator-owned in the private-cloud form: when the
// engine key rides the managed overlay the whole section locks (the
// domain is configured in the config console, not on the bridge).
const locked = () => settings.isManaged('memory_engine')
</script>

<template>
  <!-- Memory (ladyM factual memory engine) -->
  <section class="setting-section">
    <div class="setting-section-title"><Brain :size="13" style="vertical-align: -2px" /> {{ t('settings.memory.title') }}</div>
    <p class="mem-intro">{{ t('settings.memory.intro') }}</p>

    <div class="setting-row">
      <div>
        <div class="row-label">{{ t('settings.memory.engine') }}</div>
        <div class="row-hint">{{ t('settings.memory.engineHint') }}</div>
      </div>
      <select v-model="settings.memoryEngine" class="field" style="max-width: 200px" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined">
        <option value="ladym">{{ t('settings.memory.enabledOption') }}</option>
        <!-- remote: the private cloud's ladyM service (config-plane wired;
             the effective engine on managed bridges). -->
        <option value="remote">{{ t('settings.memory.remoteOption') }}</option>
        <option value="none">{{ t('settings.memory.offOption') }}</option>
      </select>
    </div>

    <template v-if="settings.memoryEngine === 'ladym'">
      <div class="setting-row">
        <div>
          <div class="row-label">{{ t('settings.memory.autoRetain') }}</div>
          <div class="row-hint">{{ t('settings.memory.autoRetainHint') }}</div>
        </div>
        <button
          class="switch"
          :class="{ on: settings.ladymAutoRetain }"
          @click="!locked() && (settings.ladymAutoRetain = !settings.ladymAutoRetain)"
        ></button>
      </div>

      <div class="setting-row">
        <div>
          <div class="row-label">{{ t('settings.memory.system2') }}</div>
          <div class="row-hint">{{ t('settings.memory.system2Hint') }}</div>
        </div>
        <button
          class="switch"
          :class="{ on: settings.ladymSystem2 }"
          @click="!locked() && (settings.ladymSystem2 = !settings.ladymSystem2)"
        ></button>
      </div>

      <div class="setting-row">
        <div>
          <div class="row-label">{{ t('settings.memory.interval') }}</div>
          <div class="row-hint">{{ t('settings.memory.intervalHint') }}</div>
        </div>
        <input
          v-model.number="settings.ladymSystem2Interval" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined"
          class="field"
          style="max-width: 100px"
          type="number"
          min="60"
          step="30"
        />
      </div>

      <div class="setting-row">
        <div>
          <div class="row-label">{{ t('settings.memory.topK') }}</div>
          <div class="row-hint">{{ t('settings.memory.topKHint') }}</div>
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
        <div class="row-label">{{ t('settings.memory.model') }}</div>
        <div class="row-hint">{{ t('settings.memory.modelHint') }}</div>

        <div class="setting-row">
          <div>
            <div class="row-label">Embedding</div>
            <div class="row-hint">{{ t('settings.memory.embeddingHint') }}</div>
          </div>
          <select v-model="settings.ladymEmbedding" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined" class="field" style="max-width: 200px">
            <option value="inherit">{{ t('settings.memory.inheritOption') }}</option>
            <option value="hashing">{{ t('settings.memory.hashingOption') }}</option>
            <option value="openai">openai</option>
            <option value="ollama">ollama</option>
            <option value="http">http</option>
          </select>
        </div>

        <div class="setting-row">
          <div>
            <div class="row-label">{{ t('settings.memory.embeddingModel') }}</div>
            <div class="row-hint">{{ t('settings.memory.embeddingModelHint') }}</div>
          </div>
          <input
            v-model="settings.ladymEmbeddingModel" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined"
            class="field"
            style="max-width: 260px"
            placeholder="text-embedding-3-small"
          />
        </div>

        <div class="setting-row">
          <div>
            <div class="row-label">Embedding Base URL</div>
            <div class="row-hint">{{ t('settings.memory.embeddingBaseHint') }}</div>
          </div>
          <input
            v-model="settings.ladymEmbeddingBaseURL" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined"
            class="field"
            style="max-width: 260px"
            :placeholder="t('settings.memory.inheritOpenAIBase')"
          />
        </div>

        <div class="setting-row">
          <div>
            <div class="row-label">Embedding API Key</div>
            <div class="row-hint">
              {{
                settings.ladymEmbeddingKeySet
                  ? t('settings.shared.setEncrypted')
                  : t('settings.memory.embeddingKeyNotSet')
              }}
            </div>
          </div>
          <input
            v-model="settings.ladymEmbeddingAPIKey" :disabled="locked()" :title="locked() ? t('settings.managedTooltip') : undefined"
            class="field"
            style="max-width: 260px"
            type="password"
            autocomplete="off"
            :placeholder="t('settings.memory.enterNew')"
          />
        </div>

        <div class="setting-row">
          <div>
            <div class="row-label">{{ t('settings.memory.llm') }}</div>
            <div class="row-hint">{{ t('settings.memory.llmHint') }}</div>
          </div>
          <select v-model="settings.ladymLLM" class="field" style="max-width: 200px">
            <option value="inherit">{{ t('settings.memory.inheritOption') }}</option>
            <option value="none">none</option>
          </select>
        </div>
      </div>

      <div class="setting-row">
        <div>
          <div class="row-label">{{ t('settings.memory.dbPath') }}</div>
          <div class="row-hint">{{ t('settings.memory.dbPathHint') }}</div>
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
