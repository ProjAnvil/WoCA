<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { s, setDraft, save, saving } = injectCfg()
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('msgplane.title') }}</div>
    <p class="hint">{{ t('msgplane.intro') }}</p>
    <div class="row">
      <label>{{ t('msgplane.mode') }}</label>
      <select class="field" :value="s?.msg_plane_mode" @change="setDraft('msg_plane_mode', ($event.target as HTMLSelectElement).value)">
        <option value="pg">{{ t('msgplane.modePG') }}</option>
        <option value="pg+nats">{{ t('msgplane.modePGNATS') }}</option>
      </select>
    </div>
    <div class="row">
      <label>{{ t('msgplane.url') }}</label>
      <input class="field" :value="s?.msg_plane_nats_url" placeholder="nats://host:4222" @input="setDraft('msg_plane_nats_url', ($event.target as HTMLInputElement).value)" />
    </div>
    <div class="row">
      <label>{{ t('msgplane.subject') }}</label>
      <input class="field" :value="s?.msg_plane_subject" placeholder="runs.dispatch" @input="setDraft('msg_plane_subject', ($event.target as HTMLInputElement).value)" />
    </div>
    <button class="primary" :disabled="saving" @click="save()">{{ t('common.save') }}</button>
  </section>
</template>
