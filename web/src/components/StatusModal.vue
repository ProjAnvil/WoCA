<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { Activity, Download, X } from "lucide-vue-next";
import { stats, type StatsResponse } from "../api/client";
import { useSettingsStore } from "../stores/settings";
import { useServerSettingsStore } from "../stores/serverSettings";
import { useStatusStore } from "../stores/status";

// The status health page (product design §6.3): connection zones — the
// gateway channel with its config version and freshness — plus the
// bridge's own identity. On un-managed deployments every zone reads
// "standalone". The snapshot comes from the shared status store (the
// sidebar poll keeps it warm); the runtime zone is local and best-effort.
const { t } = useI18n();
defineEmits<{ close: [] }>();

const statusStore = useStatusStore();
const st = computed(() => statusStore.status);
const failed = computed(() => statusStore.failed);
const runtime = ref<StatsResponse | null>(null);
const settings = useSettingsStore();
const serverSettings = useServerSettingsStore();

onMounted(async () => {
  await statusStore.refresh();
  // The runtime zone is best-effort: local/personal deployments answer
  // both, so a failing stats call never masks the connection zones.
  try {
    runtime.value = await stats();
  } catch {
    runtime.value = null;
  }
});

function freshness(): "ok" | "stale" | "off" {
  const c = st.value?.cloud;
  if (!c?.enrolled) return "off";
  return c.stale ? "stale" : "ok";
}
const cloud = computed(() => st.value?.cloud);

const lamp = {
  ok: "var(--ok)",
  stale: "var(--warn)",
  off: "var(--muted)",
} as const;

// One-click diagnostics export (product design §6.3): a Markdown bundle
// operators can paste into an issue.
function exportDiagnostics() {
  const c = st.value?.cloud;
  const md = [
    "# woca bridge diagnostics",
    "",
    `- generated: ${new Date().toISOString()}`,
    `- edition: ${st.value?.edition ?? "unknown"}`,
    `- auth posture: ${st.value?.auth ?? "unknown"}`,
    `- managed config: ${st.value?.managed ? "yes" : "no"}`,
    `- edge registered: ${c?.enrolled ? "yes" : "no"}`,
    ...(c?.enrolled
      ? [
          `- edge url: ${c.url} (source: ${c.url_source})`,
          `- bridge id: ${c.bridge_id}`,
          `- config version: ${c.last_version || "—"}`,
          `- last successful sync: ${c.last_pull || "—"}`,
          `- freshness: ${c.stale ? "STALE (serving cache)" : "fresh"}`,
          `- spki pinned: ${c.spki_pinned ? "yes" : "no"}`,
          `- tunnel: ${c.tunnel.serving ? `serving (${c.tunnel.served} requests)` : "off"}`,
        ]
      : []),
    `- url: ${window.location.href}`,
    `- user agent: ${navigator.userAgent}`,
    "",
  ].join("\n");
  const blob = new Blob([md], { type: "text/markdown" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = "woca-bridge-diagnostics.md";
  a.click();
  URL.revokeObjectURL(a.href);
}
</script>

<template>
  <div class="modal-backdrop" @click.self="$emit('close')">
    <div class="status-modal">
      <div class="card-head">
        <span class="card-title"
          ><Activity :size="15" /> {{ t("status.title") }}</span
        >
        <div style="display: flex; gap: 4px">
          <button
            class="icon-btn"
            :title="t('status.exportDiagnostics')"
            @click="exportDiagnostics"
          >
            <Download :size="15" />
          </button>
          <button
            class="icon-btn"
            :title="t('common.close')"
            @click="$emit('close')"
          >
            <X :size="15" />
          </button>
        </div>
      </div>

      <div v-if="failed" class="status-error">{{ t("status.unavailable") }}</div>
      <template v-else-if="st">
        <div v-if="cloud" class="status-zone">
          <div class="zone-title">
            <span
              class="lamp"
              :style="{
                background:
                  freshness() === 'ok'
                    ? lamp.ok
                    : freshness() === 'stale'
                      ? lamp.stale
                      : lamp.off,
              }"
            />
            {{ t("status.gateway") }}
          </div>
          <dl>
            <dt>{{ t("status.registered") }}</dt>
            <dd>
              {{
                cloud.enrolled ? t("common.yes") : t("status.standalone")
              }}
            </dd>
            <template v-if="cloud.enrolled">
              <dt>{{ t("status.bridgeId") }}</dt>
              <dd>{{ cloud.bridge_id }}</dd>
              <dt>{{ t("status.configVersion") }}</dt>
              <dd>{{ cloud.last_version || "—" }}</dd>
              <dt>{{ t("status.lastSync") }}</dt>
              <dd>{{ cloud.last_pull || "—" }}</dd>
              <dt>{{ t("status.freshness") }}</dt>
              <dd>
                {{
                  cloud.stale ? t("status.staleCache") : t("status.fresh")
                }}
              </dd>
              <dt>{{ t("status.tunnel") }}</dt>
              <dd>
                {{
                  cloud.tunnel.serving
                    ? t("status.tunnelServing", { n: cloud.tunnel.served })
                    : t("status.tunnelOff")
                }}
              </dd>
            </template>
          </dl>
        </div>
        <div v-if="runtime" class="status-zone">
          <div class="zone-title">{{ t("status.runtime") }}</div>
          <dl>
            <dt>{{ t("status.activeRuns") }}</dt>
            <dd>{{ runtime.active_runs }}</dd>
            <dt>{{ t("status.queuedRuns") }}</dt>
            <dd>{{ runtime.queued_runs }}</dd>
            <dt>{{ t("status.teammates") }}</dt>
            <dd>{{ runtime.teammates }}</dd>
            <dt>{{ t("status.droppedEvents") }}</dt>
            <dd>{{ runtime.sse_dropped }}</dd>
          </dl>
        </div>
        <div class="status-zone">
          <div class="zone-title">{{ t("status.governance") }}</div>
          <dl>
            <dt>{{ t("status.edition") }}</dt>
            <dd>{{ st.edition }}</dd>
            <dt>{{ t("status.managedConfig") }}</dt>
            <dd>
              {{ st.managed ? t("status.managedOn") : t("status.localOnly") }}
            </dd>
          </dl>
          <dl v-if="serverSettings.managedFields.length" class="status-managed-list">
            <dt v-for="f in serverSettings.managedFields" :key="f">
              {{ f }}
              <span class="status-managed-src">managed{{ cloud?.last_version ? " · " + cloud.last_version : "" }}</span>
            </dt>
          </dl>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.status-modal {
  width: min(480px, 92vw);
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--panel);
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-weight: 600;
}
.card-title {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.status-zone {
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 10px 12px;
}
.zone-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 600;
  margin-bottom: 8px;
  font-size: 13px;
}
dl {
  display: grid;
  grid-template-columns: 130px 1fr;
  gap: 4px 10px;
  margin: 0;
  font-size: 12.5px;
}
dt {
  opacity: 0.65;
}
dd {
  margin: 0;
  font-family: ui-monospace, monospace;
  overflow-wrap: anywhere;
}
.status-error {
  color: var(--danger);
  font-size: 13px;
}
</style>
