<script setup lang="ts">
import { ref } from "vue";
import { useI18n } from "vue-i18n";
import { KeyRound, LockKeyhole, QrCode } from "lucide-vue-next";
import QRCode from "qrcode";
import { login, UnauthorizedError } from "../api/client";

const { t } = useI18n();
const props = defineProps<{ revoked?: boolean }>();
const emit = defineEmits<{ authenticated: [] }>();

const apiKey = ref("");
const busy = ref(false);
const error = ref("");
const qrData = ref("");
const showQR = ref(false);

// Pair a phone (product design P3): the QR carries the bridge URL; the
// phone scans, opens the login page, and pastes its key.
async function toggleQR() {
  showQR.value = !showQR.value;
  if (showQR.value && !qrData.value) {
    qrData.value = await QRCode.toDataURL(window.location.origin, {
      margin: 1,
      width: 180,
    });
  }
}

async function submit() {
  const key = apiKey.value.trim();
  if (!key || busy.value) return;
  busy.value = true;
  error.value = "";
  try {
    await login(key);
    // The cookie carries the session; the key is not stored anywhere.
    apiKey.value = "";
    emit("authenticated");
  } catch (e) {
    error.value =
      e instanceof UnauthorizedError ||
      (e instanceof Error && e.message.includes("401"))
        ? t("login.rejected")
        : e instanceof Error
          ? e.message
          : String(e);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="login-gate-overlay">
    <form class="login-gate" @submit.prevent="submit">
      <div class="login-gate-brand">
        <div class="brand-mark">W</div>
        <span>woca</span>
      </div>
      <p class="login-gate-tagline">{{ t("login.tagline") }}</p>
      <p v-if="props.revoked" class="login-gate-revoked" data-testid="revoked-notice" role="alert">
        {{ t("login.revoked") }}
      </p>
      <p class="login-gate-hint">
        <LockKeyhole :size="14" />
        {{ t("login.hint") }}
      </p>
      <div class="login-gate-row">
        <KeyRound :size="16" />
        <input
          v-model="apiKey"
          type="password"
          autocomplete="off"
          :placeholder="t('login.keyPlaceholder')"
          autofocus
        />
      </div>
      <button
        class="btn btn-primary"
        type="submit"
        :disabled="!apiKey.trim() || busy"
      >
        {{ t("login.submit") }}
      </button>
      <p v-if="error" class="login-gate-error">{{ error }}</p>
      <button class="login-gate-qr" type="button" @click="toggleQR">
        <QrCode :size="14" />
        {{ t("login.pairQR") }}
      </button>
      <img
        v-if="showQR && qrData"
        :src="qrData"
        alt="bridge URL QR"
        class="login-gate-qr-img"
      />
      <p v-if="showQR && qrData" class="login-gate-qr-hint">
        {{ t("login.pairQRHint") }}
      </p>
    </form>
  </div>
</template>

<style scoped>
.login-gate-overlay {
  position: fixed;
  inset: 0;
  z-index: 200;
  background: var(--bg);
  display: flex;
  align-items: center;
  justify-content: center;
}
.login-gate {
  display: flex;
  flex-direction: column;
  gap: 14px;
  width: min(360px, 90vw);
  padding: 28px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--panel);
}
.login-gate-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 600;
  font-size: 18px;
}
.login-gate-brand .brand-mark {
  width: 28px;
  height: 28px;
  border-radius: 8px;
  background: var(--brand);
  color: var(--on-brand);
  display: flex;
  align-items: center;
  justify-content: center;
}
.login-gate-tagline {
  margin: -8px 0 0;
  font-size: 12px;
}
.login-gate-revoked {
  margin: 0;
  padding: 8px 10px;
  border-radius: 8px;
  background: rgba(229, 83, 75, 0.14);
  color: var(--danger);
  font-size: 12.5px;
}
.login-gate-hint {
  display: flex;
  gap: 6px;
  align-items: center;
  margin: 0;
  font-size: 12px;
  opacity: 0.75;
}
.login-gate-row {
  display: flex;
  gap: 8px;
  align-items: center;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 8px 10px;
}
.login-gate-row input {
  flex: 1;
  background: transparent;
  border: none;
  outline: none;
  color: inherit;
  font: inherit;
}
.login-gate-error {
  margin: 0;
  color: var(--danger);
  font-size: 12px;
}
.login-gate-qr {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: none;
  border: none;
  color: var(--brand);
  cursor: pointer;
  font: inherit;
  font-size: 12.5px;
  padding: 0;
  align-self: center;
}
.login-gate-qr-img {
  align-self: center;
  border-radius: 8px;
  background: #fff;
  padding: 6px;
}
.login-gate-qr-hint {
  margin: 0;
  align-self: center;
  font-size: 12px;
  opacity: 0.75;
  text-align: center;
}
</style>
