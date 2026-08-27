<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Plug } from 'lucide-vue-next'
import { getNativeBridge } from '../../native/bridge'
import WInfoTip from '../ui/WInfoTip.vue'

// The native-only connection switcher: dropping the active profile's
// session returns the app to the connect view (saved profiles, the
// embedded bridge, adding a new one) — the same shape as the web's
// access-point switching, adapted to the app's own profile model. The
// binding revokes remotely (best effort) and removes the local session
// from the app database; the reload re-runs Bootstrap into the gate.
const { t } = useI18n()

async function disconnect() {
  try {
    await getNativeBridge()?.Logout()
  } catch {
    // A dead channel still logged out locally; the reload decides the gate.
  }
  window.location.reload()
}
</script>

<template>
  <section class="setting-section">
    <div class="setting-section-title">
      <Plug :size="13" style="vertical-align: -2px" /> {{ t('settings.nativeConnect.title') }}
    </div>
    <p class="mem-intro">{{ t('settings.nativeConnect.intro') }}</p>

    <div class="setting-row">
      <div>
<div class="row-label">{{ t('settings.nativeConnect.button') }}<WInfoTip :text="t('settings.nativeConnect.hint')" /></div>
      </div>
      <button class="btn" data-testid="native-disconnect" @click="disconnect()">
        {{ t('settings.nativeConnect.button') }}
      </button>
    </div>
  </section>
</template>
