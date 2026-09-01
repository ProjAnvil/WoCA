<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from "vue";
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

// Entrance choreography: while the card "develops" (blur → sharp) the
// programmatic autofocus would light the field ring before its content is
// visible. The class suppresses the ring for the ~650ms entrance window —
// styling only, no behavioral contract.
const entering = ref(true);
let enteringTimer = 0;
onMounted(() => {
  enteringTimer = window.setTimeout(() => (entering.value = false), 650);
});
onBeforeUnmount(() => window.clearTimeout(enteringTimer));

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

// Retyping clears the rejection: the error settles away and the field drops
// its danger border — feedback is "start over", not a lingering accusation.
function onKeyInput() {
  if (error.value) error.value = "";
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
    <form class="login-gate" :class="{ 'is-entering': entering }" @submit.prevent="submit">
      <div class="gate-head">
        <div class="login-gate-brand rise" :style="{ '--d': '120ms' }">
          <div class="brand-mark">W</div>
          <span>woca</span>
        </div>
        <p class="login-gate-tagline rise" :style="{ '--d': '160ms' }">{{ t("login.tagline") }}</p>
      </div>

      <!-- Revocation notice: reveals (grid + cross-fade + rail growth) instead
           of popping in; role=alert semantics are unaffected by the motion. -->
      <div class="revoked-wrap" :class="{ open: props.revoked }">
        <div>
          <p
            v-if="props.revoked"
            class="login-gate-revoked"
            data-testid="revoked-notice"
            role="alert"
          >
            {{ t("login.revoked") }}
          </p>
        </div>
      </div>

      <p class="login-gate-hint rise" :style="{ '--d': '240ms' }">
        <LockKeyhole :size="14" />
        {{ t("login.hint") }}
      </p>

      <div class="login-gate-row rise" :class="{ error: !!error }" :style="{ '--d': '280ms' }">
        <KeyRound :size="16" class="row-icon" />
        <input
          v-model="apiKey"
          type="password"
          autocomplete="off"
          :placeholder="t('login.keyPlaceholder')"
          autofocus
          @input="onKeyInput"
        />
      </div>

      <!-- Rejection settles in below the field (source → consequence), and
           settles away on the next keystroke. -->
      <div class="err-wrap" :class="{ open: !!error }">
        <div>
          <p class="login-gate-error" :aria-hidden="!error">{{ error }}</p>
        </div>
      </div>

      <button
        class="btn btn-primary login-submit rise"
        :style="{ '--d': '320ms' }"
        type="submit"
        :disabled="!apiKey.trim() || busy"
      >
        <span class="btn-label">{{ t("login.submit") }}</span>
        <span class="btn-spinner" aria-hidden="true"></span>
      </button>

      <button
        class="login-gate-qr rise"
        :class="{ open: showQR }"
        :style="{ '--d': '360ms' }"
        type="button"
        @click="toggleQR"
      >
        <QrCode :size="14" />
        {{ t("login.pairQR") }}
      </button>

      <!-- Structural wrapper only: the wrap stays mounted so the grid reveal
           can animate; the img keeps its async-ready v-if inside. -->
      <div class="qr-wrap" :class="{ open: showQR }">
        <div>
          <div class="qr-body">
            <img
              v-if="showQR && qrData"
              :src="qrData"
              alt="bridge URL QR"
              class="login-gate-qr-img"
            />
            <p v-if="showQR" class="login-gate-qr-hint">{{ t("login.pairQRHint") }}</p>
          </div>
        </div>
      </div>
    </form>
  </div>
</template>

<style scoped>
/* Stage: a dark room — radial light pooling behind the card, grain on top to
   keep the grayscale gradient reading as material (tokens: --gate-*). */
.login-gate-overlay {
  position: fixed;
  inset: 0;
  z-index: 200;
  display: flex;
  align-items: center;
  justify-content: center;
  background: radial-gradient(
    120% 90% at 50% 42%,
    var(--gate-bg-glow) 0%,
    var(--bg) 46%,
    var(--gate-bg-edge) 100%
  );
}
.login-gate-overlay::before {
  content: "";
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image: var(--gate-noise);
  background-size: 180px 180px;
  opacity: var(--gate-noise-opacity);
  mix-blend-mode: overlay;
}

/* Sheet of paper: top-lit vertical gradient + 1px edge + inner top highlight
   (the dark-UI glaze trick) + the shared ambient shadow. */
.login-gate {
  position: relative;
  display: flex;
  flex-direction: column;
  width: min(360px, 90vw);
  padding: 32px;
  border: 1px solid var(--border);
  border-radius: var(--radius-2xl);
  background: linear-gradient(
    180deg,
    var(--gate-card-hi) 0%,
    var(--panel) 38%,
    var(--gate-card-lo) 100%
  );
  box-shadow: inset 0 1px 0 var(--gate-inner-hi), var(--shadow-md);
}

/* --- entrance: the card develops (blur → sharp), contents surface in a
   40ms-step stagger; only the card gets blur (performance rule) --- */
@keyframes gate-card-in {
  from {
    opacity: 0;
    transform: translateY(12px) scale(0.985);
    filter: blur(10px);
  }
  to {
    opacity: 1;
    transform: translateY(0) scale(1);
    filter: blur(0);
  }
}
@keyframes gate-item-in {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}
.login-gate {
  animation: gate-card-in 480ms var(--ease-out) both;
}
.login-gate .rise {
  animation: gate-item-in 360ms var(--ease-out) both;
  animation-delay: var(--d, 0ms);
}
.login-gate.is-entering .login-gate-row {
  box-shadow: none !important;
}

/* --- brand: an ink block with a glaze, like a seal on the sheet --- */
.gate-head {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  margin-bottom: var(--space-6);
}
.login-gate-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 600;
  font-size: var(--text-lg);
  line-height: var(--lh-lg);
}
.login-gate-brand .brand-mark {
  width: 28px;
  height: 28px;
  border-radius: var(--radius-xl);
  background: linear-gradient(180deg, var(--brand-mark-hi), var(--brand-mark-lo));
  color: var(--on-brand);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 15px;
  font-weight: 700;
  box-shadow: inset 0 1px 0 var(--gate-inner-hi), var(--shadow-sm);
}
.login-gate-tagline {
  margin: 0;
  font-size: var(--text-xs);
  line-height: var(--lh-xs);
  color: var(--text-2);
  letter-spacing: 0.02em;
}

/* --- revocation: reveal + left rail growth; red is signal, not area --- */
.revoked-wrap {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows var(--dur-slow) var(--ease-in-out);
}
.revoked-wrap.open {
  grid-template-rows: 1fr;
  margin-bottom: var(--space-3);
}
.revoked-wrap > div {
  min-height: 0;
  overflow: hidden;
}
.login-gate-revoked {
  position: relative;
  margin: 0;
  padding: 8px 10px;
  border-radius: var(--radius);
  background: var(--danger-soft);
  color: var(--danger);
  font-size: 12.5px;
  line-height: 18px;
  opacity: 0;
  transform: translateY(-4px);
  transition:
    opacity var(--dur-base) var(--ease-out) 40ms,
    transform var(--dur-base) var(--ease-out) 40ms;
}
.revoked-wrap.open .login-gate-revoked {
  opacity: 1;
  transform: none;
}
.login-gate-revoked::after {
  content: "";
  position: absolute;
  left: 0;
  top: 0;
  bottom: 0;
  width: 2px;
  border-radius: 2px 0 0 2px;
  background: var(--danger);
  transform: scaleY(0);
  transform-origin: top;
  transition: transform var(--dur-slow) var(--ease-out);
}
.revoked-wrap.open .login-gate-revoked::after {
  transform: scaleY(1);
}

/* --- hint --- */
.login-gate-hint {
  display: flex;
  gap: 6px;
  align-items: center;
  margin: 0 0 var(--space-3);
  font-size: var(--text-xs);
  line-height: 18px;
  color: var(--muted);
}

/* --- field: recessed against the raised sheet; focus adds light (ink ring),
   not color --- */
.login-gate-row {
  display: flex;
  gap: var(--space-2);
  align-items: center;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 9px 10px;
  background: var(--gate-field);
  margin-bottom: var(--space-3);
  transition:
    border-color var(--dur-base) var(--ease-out),
    box-shadow var(--dur-base) var(--ease-out);
}
.login-gate-row:focus-within {
  border-color: var(--focus-border);
  box-shadow: 0 0 0 3px var(--focus-ring);
}
.login-gate-row.error {
  border-color: color-mix(in srgb, var(--danger) 45%, transparent);
}
.login-gate-row .row-icon {
  flex: none;
  color: var(--muted);
}
.login-gate-row input {
  flex: 1;
  min-width: 0;
  background: transparent;
  border: none;
  outline: none;
  color: var(--text);
  font: inherit;
}
.login-gate-row input::placeholder {
  color: var(--muted);
}

/* --- rejection settles below the field; no shake (calm interface) --- */
.err-wrap {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows 160ms var(--ease-in-out);
}
.err-wrap.open {
  grid-template-rows: 1fr;
  margin-bottom: var(--space-1);
}
.err-wrap > div {
  min-height: 0;
  overflow: hidden;
}
.login-gate-error {
  margin: 0;
  padding: 2px 2px 6px;
  color: var(--danger);
  font-size: var(--text-xs);
  line-height: 18px;
  opacity: 0;
  transform: translateY(-4px);
  transition:
    opacity var(--dur-base) var(--ease-out) 40ms,
    transform var(--dur-base) var(--ease-out) 40ms;
}
.err-wrap.open .login-gate-error {
  opacity: 1;
  transform: none;
}

/* --- submit: disabled sleeps at 0.4, wakes over 240ms on the first pasted
   character; busy cross-fades label ↔ spinner with zero layout shift --- */
.login-submit {
  position: relative;
  height: 36px;
  font-size: var(--text-base);
  font-weight: 500;
  transition:
    opacity var(--dur-slow) var(--ease-in-out),
    background-color 160ms var(--ease-out),
    transform 160ms var(--ease-out),
    box-shadow var(--dur-base) var(--ease-out);
}
.login-submit:disabled {
  opacity: 0.4;
  cursor: default;
}
.login-submit:not(:disabled):hover {
  transform: translateY(-1px);
}
.login-submit:not(:disabled):active {
  transform: translateY(0) scale(0.985);
}
.login-submit:focus-visible {
  outline: none;
  box-shadow: 0 0 0 3px var(--focus-ring);
}
.login-submit .btn-label {
  display: inline-flex;
  transition: opacity var(--dur-base) var(--ease-out);
}
.login-submit.busy .btn-label {
  opacity: 0;
}
.btn-spinner {
  position: absolute;
  inset: 0;
  margin: auto;
  width: 14px;
  height: 14px;
  border-radius: var(--radius-full);
  border: 2px solid color-mix(in srgb, var(--on-accent) 25%, transparent);
  border-top-color: var(--on-accent);
  opacity: 0;
  pointer-events: none;
  transition: opacity var(--dur-base) var(--ease-out);
}
.login-submit.busy .btn-spinner {
  opacity: 1;
  animation: gate-spin 700ms linear infinite;
}
@keyframes gate-spin {
  to {
    transform: rotate(360deg);
  }
}

/* --- QR pairing: link stays quiet; the wrap reveals via grid rows while the
   image alone gets a focus-style blur-to-sharp — a nod to scanning --- */
.login-gate-qr {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: none;
  border: none;
  color: var(--muted);
  cursor: pointer;
  font: inherit;
  font-size: var(--text-sm);
  line-height: 18px;
  padding: var(--space-1) 0 0;
  align-self: center;
  transition: color var(--dur-fast) var(--ease-out);
}
.login-gate-qr:hover {
  color: var(--text-2);
}
.qr-wrap {
  display: grid;
  grid-template-rows: 0fr;
  transition: grid-template-rows 160ms var(--ease-in-out);
}
.qr-wrap.open {
  grid-template-rows: 1fr;
}
.qr-wrap > div {
  min-height: 0;
  overflow: hidden;
}
.qr-body {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-2);
  padding-top: var(--space-3);
  opacity: 0;
  transform: translateY(-6px);
  transition:
    opacity var(--dur-base) var(--ease-out) 60ms,
    transform var(--dur-base) var(--ease-out) 60ms;
}
.qr-wrap.open .qr-body {
  opacity: 1;
  transform: none;
}
.login-gate-qr-img {
  border-radius: var(--radius-xl);
  background: #fff; /* scanning substrate — functional white, not a theme color */
  padding: 8px;
  border: 1px solid var(--border);
  filter: blur(8px);
  opacity: 0;
  transform: scale(0.97);
  transition:
    filter 260ms var(--ease-out) 80ms,
    opacity 260ms var(--ease-out) 80ms,
    transform 260ms var(--ease-out) 80ms;
}
.qr-wrap.open .login-gate-qr-img {
  filter: blur(0);
  opacity: 1;
  transform: none;
}
.login-gate-qr-hint {
  margin: 0;
  font-size: var(--text-xs);
  line-height: 18px;
  color: var(--muted);
  text-align: center;
  max-width: 240px;
}
</style>
