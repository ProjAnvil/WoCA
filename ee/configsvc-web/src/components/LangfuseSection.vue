<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { s, d, setDraft, save, saving } = injectCfg()

function setEnabled(on: boolean) {
  setDraft('langfuse_enabled', on ? 'true' : 'false')
}
const enabled = () => (d('langfuse_enabled') !== undefined ? d('langfuse_enabled') === 'true' : Boolean(s?.langfuse_enabled))
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('langfuse.title') }}</div>
    <p class="hint">{{ t('langfuse.intro') }}</p>
    <div class="row">
      <label>{{ t('langfuse.enabled') }}</label>
      <button
        class="switch"
        role="switch"
        type="button"
        :class="{ on: enabled() }"
        :aria-checked="enabled()"
        @click="setEnabled(!enabled())"
      ></button>
    </div>
    <template v-if="enabled()">
      <div class="row">
        <label>{{ t('langfuse.baseURL') }}</label>
        <input
          class="field"
          :value="s?.langfuse_base_url"
          placeholder="https://cloud.langfuse.com"
          @input="setDraft('langfuse_base_url', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <div class="row">
        <label>{{ t('langfuse.publicKey') }}</label>
        <input
          class="field"
          :value="s?.langfuse_public_key"
          placeholder="pk-lf-…"
          @input="setDraft('langfuse_public_key', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <div class="row">
        <label>{{ t('langfuse.secretKey') }}</label>
        <input
          class="field"
          type="password"
          autocomplete="new-password"
          :placeholder="s?.langfuse_secret_key_set ? t('langfuse.secretSet') : 'sk-lf-…'"
          @input="setDraft('langfuse_secret_key', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <p class="hint">{{ t('langfuse.hint') }}</p>
    </template>
    <button class="primary" :disabled="saving" @click="save()">{{ t('common.save') }}</button>
  </section>
</template>
