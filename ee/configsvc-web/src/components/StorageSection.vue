<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { injectCfg } from '../inject'
const { t } = useI18n()
const { s, v, setDraft, save, saving } = injectCfg()
</script>

<template>
  <section class="card">
    <div class="sec-title">{{ t('nav.storage') }}</div>

    <!-- Checkpoint backend -->
    <div class="row">
      <label>{{ t('storage.checkpoint') }}</label>
      <select class="field" :value="v('checkpoint_backend')" @change="setDraft('checkpoint_backend', ($event.target as HTMLSelectElement).value)">
        <!-- Enterprise plane: postgres-grade only (product ruling). -->
        <option value="postgres">postgres</option>
      </select>
    </div>
    <div v-if="v('checkpoint_backend') === 'file'" class="row sub">
      <label>{{ t('storage.dir') }}</label>
      <input class="field" :value="v('checkpoint_dir')" @input="setDraft('checkpoint_dir', ($event.target as HTMLInputElement).value)" />
    </div>
    <template v-if="v('checkpoint_backend') === 'postgres'">
      <div class="row sub">
        <label>{{ t('storage.host') }}</label>
        <input class="field" :value="v('checkpoint_pg_host')" placeholder="pg-primary" @input="setDraft('checkpoint_pg_host', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.port') }}</label>
        <input class="field" :value="v('checkpoint_pg_port') || '5432'" @input="setDraft('checkpoint_pg_port', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.dbname') }}</label>
        <input class="field" :value="v('checkpoint_pg_db') || 'woca'" @input="setDraft('checkpoint_pg_db', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.user') }}</label>
        <input class="field" :value="v('checkpoint_pg_user')" @input="setDraft('checkpoint_pg_user', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.password') }}</label>
        <input
          class="field"
          type="password"
          :placeholder="s?.checkpoint_pg_password_set ? t('storage.pwSet') : t('storage.pwPlaceholder')"
          @input="setDraft('checkpoint_pg_password', ($event.target as HTMLInputElement).value)"
        />
      </div>
    </template>

    <!-- MemoryStore backend -->
    <div class="row" style="margin-top: 10px">
      <label>{{ t('storage.memoryStore') }}</label>
      <select class="field" :value="v('memory_backend')" @change="setDraft('memory_backend', ($event.target as HTMLSelectElement).value)">
        <!-- Enterprise plane: postgres-grade only (product ruling). -->
        <option value="postgres">postgres</option>
      </select>
    </div>
    <template v-if="v('memory_backend') === 'postgres'">
      <div class="row sub">
        <label>{{ t('storage.host') }}</label>
        <input class="field" :value="v('memory_pg_host')" placeholder="pg-primary" @input="setDraft('memory_pg_host', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.port') }}</label>
        <input class="field" :value="v('memory_pg_port') || '5432'" @input="setDraft('memory_pg_port', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.dbname') }}</label>
        <input class="field" :value="v('memory_pg_db') || 'woca'" @input="setDraft('memory_pg_db', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.user') }}</label>
        <input class="field" :value="v('memory_pg_user')" @input="setDraft('memory_pg_user', ($event.target as HTMLInputElement).value)" />
      </div>
      <div class="row sub">
        <label>{{ t('storage.password') }}</label>
        <input
          class="field"
          type="password"
          :placeholder="s?.memory_pg_password_set ? t('storage.pwSet') : t('storage.pwPlaceholder')"
          @input="setDraft('memory_pg_password', ($event.target as HTMLInputElement).value)"
        />
      </div>
    </template>

    <p class="hint">{{ t('storage.pgHint') }}</p>
    <button class="primary" :disabled="saving" @click="save()">{{ t('common.save') }}</button>
  </section>
</template>
