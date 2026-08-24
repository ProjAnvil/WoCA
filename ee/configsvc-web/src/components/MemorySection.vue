<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { s, v, setDraft, save, saving } = injectCfg()
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('nav.memory') }}</div>
    <div class="row">
      <label>{{ t('memory.engine') }}</label>
      <select class="field" :value="s?.memory_engine" @change="setDraft('memory_engine', ($event.target as HTMLSelectElement).value)">
        <option value="ladym">ladym（内嵌，个人/轻量）</option>
        <option value="remote">remote（ladyM 企业版 API）</option>
        <option value="none">none（关闭）</option>
      </select>
    </div>

    <template v-if="v('memory_engine') === 'remote'">
      <div class="row" style="margin-left: 0">
        <label>{{ t('memory.endpoint') }}</label>
        <input
          class="field"
          placeholder="http://ladym-api:8080"
          :value="s?.ladym_endpoint"
          @input="setDraft('ladym_endpoint', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <div class="row" style="margin-left: 0">
        <label>{{ t('memory.username') }}</label>
        <input
          class="field"
          :value="s?.ladym_api_username"
          @input="setDraft('ladym_api_username', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <div class="row" style="margin-left: 0">
        <label>{{ t('memory.password') }}</label>
        <input
          class="field"
          type="password"
          :placeholder="s?.ladym_api_password_set ? t('storage.pgSet') : t('memory.passwordPlaceholder')"
          @input="setDraft('ladym_api_password', ($event.target as HTMLInputElement).value)"
        />
      </div>
      <p class="hint" style="margin: 2px 0 10px">{{ t('memory.remoteHint') }}</p>
    </template>

    <div class="row">
      <label>{{ t('memory.topk') }}</label>
      <input class="field" type="number" :value="s?.ladym_topk" @input="setDraft('ladym_topk', ($event.target as HTMLInputElement).value)" />
    </div>
    <div class="row">
      <label>{{ t('memory.autoretain') }}</label>
      <input type="checkbox" :checked="s?.ladym_autoretain" @change="setDraft('ladym_autoretain', ($event.target as HTMLInputElement).checked ? 'true' : 'false')" />
    </div>
    <div class="row">
      <label>{{ t('memory.system2') }}</label>
      <input type="checkbox" :checked="s?.ladym_system2" @change="setDraft('ladym_system2', ($event.target as HTMLInputElement).checked ? 'true' : 'false')" />
    </div>
    <div class="row">
      <label>{{ t('memory.interval') }}</label>
      <input class="field" type="number" :value="s?.ladym_system2_interval" @input="setDraft('ladym_system2_interval', ($event.target as HTMLInputElement).value)" />
    </div>
    <button class="primary" :disabled="saving" @click="save()">{{ t('common.save') }}</button>
  </section>
</template>
