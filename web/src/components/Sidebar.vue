<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Activity, FolderOpen, MessageSquare, Plus, Settings, X } from 'lucide-vue-next'
import FolderPicker from "./FolderPicker.vue";
import StatusModal from "./StatusModal.vue";
import SessionList from "./SessionList.vue";
import { useSessionStore } from "../stores/sessions";
import { useSettingsStore } from "../stores/settings";
import { useStatusStore } from "../stores/status";
import { useUiStore } from "../stores/ui";
import { usePolling } from "../lib/interval";
import { baseName } from "../lib/format";
import { useFocusTrap } from "../composables/useFocusTrap";

const { t } = useI18n();
const sessions = useSessionStore();
const settings = useSettingsStore();
const statusStore = useStatusStore();

// ≤640px drawer state (hamburger in ChatPanel's header toggles it; on
// desktop shells the class is inert — the drawer styles are media-scoped).
const ui = useUiStore();

// Drawer focus trap (Tab wrap, focus return, scroll lock, Esc). The
// hamburger that sets sidebarOpen is media-scoped ≤640, but the state
// survives a live resize wider — there the drawer CSS reverts to the
// static desktop sidebar (layout.css) while an engaged trap would keep
// the body scroll-locked and Tab confined, invisibly, on a normal layout.
// So the breakpoint is tracked live (mirroring WorkspaceSidebar at 1100)
// and the trap gates on open AND drawer shell; the stale sidebarOpen
// flag itself is harmless (drawer classes/styles are media-scoped). The
// trap's Esc replaces the former hand-mounted window listener; trapStack
// layering makes trap-wired overlays above the drawer consume Esc first,
// and WocaApp's global Esc routing no longer sees it (stopPropagation).
// Hand-rolled overlays hosted here (new-workspace dialog, FolderPicker,
// StatusModal) carry no trap of their own, so the esc callback still
// yields while focus sits inside one — that overlay closes first, the
// next Esc closes the drawer.
const drawerEl = ref<HTMLElement | null>(null);
const drawerShell = ref(false);
const onShellChange = (e: MediaQueryListEvent) => {
  drawerShell.value = e.matches;
};
let shellMq: MediaQueryList | null = null;
onMounted(() => {
  if (typeof window.matchMedia !== "function") return;
  shellMq = window.matchMedia("(max-width: 640px)");
  drawerShell.value = shellMq.matches;
  shellMq.addEventListener("change", onShellChange);
});
onBeforeUnmount(() => shellMq?.removeEventListener("change", onShellChange));

// Tablet rail band (spec 2026-11-19-tablet-icon-rail D1/D5): 641-1023px
// folds the 280px column into a 56px icon rail; the sessions/workspace
// content opens as a flyout anchored to the rail (Task 2). Tracked live
// the same way as the ≤640 drawer shell — the CSS media rules are the
// authority, this observer only drives which subtree renders, so the
// drift window is one change event.
const railShell = ref(false);
const flyoutOpen = ref(false);
const onRailChange = (e: MediaQueryListEvent) => {
  railShell.value = e.matches;
};
let railMq: MediaQueryList | null = null;
onMounted(() => {
  if (typeof window.matchMedia !== "function") return;
  railMq = window.matchMedia("(min-width: 641px) and (max-width: 1023px)");
  railShell.value = railMq.matches;
  railMq.addEventListener("change", onRailChange);
});
onBeforeUnmount(() => railMq?.removeEventListener("change", onRailChange));
useFocusTrap(drawerEl, computed(() => ui.sidebarOpen && drawerShell.value), {
  esc: () => {
    if (
      document.activeElement instanceof Element &&
      document.activeElement.closest(".overlay, .modal-backdrop, .w-modal-backdrop")
    ) {
      return;
    }
    ui.closeSidebar();
  },
});

// The header "+" adds a new workspace (a directory path; the workspace
// materializes in the list via its first session). Adding a session to an
// existing workspace is the "+" on each workspace group in SessionList.
const wsDialogOpen = ref(false);
const wsPath = ref("");
const wsInput = ref<HTMLInputElement | null>(null);
const pickerOpen = ref(false);
const statusOpen = ref(false);
// One probe on mount decides whether this deployment serves the bridge
// status surface (personal `woca serve` does not, so the footer shows no
// status entry and the poll never arms). Once the surface is seen — from
// here or from the settings panel — the shared 20s poll keeps the lamp
// (and the edition snapshot) fresh.
const lampPoll = usePolling(() => statusStore.refresh(), 20000);

onMounted(() => {
  void statusStore.refresh();
});

watch(
  () => statusStore.hasStatusSurface,
  (has) => {
    if (has) lampPoll.start();
  }
);

function openWorkspaceDialog() {
  wsPath.value = "";
  wsDialogOpen.value = true;
  nextTick(() => wsInput.value?.focus());
}

async function createWorkspace() {
  const path = wsPath.value.trim().replace(/\/+$/, "");
  if (!path) return;
  wsDialogOpen.value = false;
  await sessions.createSession({ path, name: baseName(path) });
}

async function pickWorkspace(path: string) {
  pickerOpen.value = false;
  wsDialogOpen.value = false;
  await sessions.createSession({ path, name: baseName(path) });
}
</script>

<template>
  <!-- Drawer veil for the ≤640px sidebar drawer (hidden by CSS on wider
       shells); sits before the aside so the drawer paints above it. -->
  <div
    v-if="ui.sidebarOpen"
    class="sidebar-backdrop"
    aria-hidden="true"
    @click.self="ui.closeSidebar()"
  />

  <!-- Tablet-band sessions flyout (spec D3): siblings of the rail aside,
       not children — once the rail aside turns relative (rail-open, the
       z21 anchor) it would otherwise become the containing block for
       absolutely-positioned children and clip them to the 56px rail. -->
  <template v-if="railShell && flyoutOpen">
    <div
      class="flyout-scrim"
      aria-hidden="true"
      @click="flyoutOpen = false"
    />
    <aside id="rail-flyout" class="rail-flyout" :aria-label="t('sidebar.sessions')">
      <div class="flyout-head">
        <span class="flyout-title">{{ t('sidebar.sessions') }}</span>
        <div class="flyout-head-actions">
          <button
            class="icon-btn"
            :title="t('sidebar.newWorkspace')"
            :aria-label="t('sidebar.newWorkspace')"
            @click="openWorkspaceDialog"
          >
            <Plus :size="16" />
          </button>
          <button
            class="icon-btn flyout-close"
            :title="t('common.close')"
            :aria-label="t('common.close')"
            @click="flyoutOpen = false"
          >
            <X :size="15" />
          </button>
        </div>
      </div>
      <div class="flyout-body">
        <section class="side-section">
          <div class="side-section-head">
            <span class="side-section-title">{{ t('sidebar.workspaces') }}</span>
          </div>
          <SessionList :on-select-extra="() => (flyoutOpen = false)" />
        </section>
      </div>
    </aside>
  </template>

  <aside
    id="app-sidebar"
    ref="drawerEl"
    class="sidebar"
    :class="{ open: ui.sidebarOpen, 'rail-open': railShell && flyoutOpen }"
  >
    <div class="sidebar-brand">
      <div class="brand-mark">W</div>
      <span class="brand-name">woca</span>
    </div>

    <!-- Tablet band (641-1023px): the rail subtree. The 280px column's
         content (workspace section + session list) does not render here —
         it opens as a flyout anchored to the rail. The dialog cluster at
         the shared aside tail stays OUTSIDE this fork so the rail buttons
         and the 280px header "+" drive the very same single-instance
         dialogs. -->
    <nav v-if="railShell" class="rail-nav" :aria-label="t('sidebar.workspaces')">
      <button
        class="rail-btn"
        :class="{ active: flyoutOpen }"
        :aria-expanded="flyoutOpen"
        aria-controls="rail-flyout"
        :title="t('sidebar.sessions')"
        :aria-label="t('sidebar.sessions')"
        @click="flyoutOpen = !flyoutOpen"
      >
        <MessageSquare :size="17" />
      </button>
      <button
        class="rail-btn"
        :title="t('sidebar.newWorkspace')"
        :aria-label="t('sidebar.newWorkspace')"
        @click="openWorkspaceDialog"
      >
        <Plus :size="17" />
      </button>
      <button
        v-if="statusStore.hasStatusSurface"
        class="rail-btn"
        :title="t('status.entry')"
        :aria-label="t('status.entry')"
        @click="statusOpen = true"
      >
        <span class="rail-lamp" :class="'lamp-' + statusStore.lamp" />
        <Activity :size="17" />
      </button>
      <div class="rail-spacer" />
      <button
        class="rail-btn"
        :title="t('sidebar.settings')"
        :aria-label="t('sidebar.settings')"
        @click="settings.openPanel()"
      >
        <Settings :size="17" />
      </button>
    </nav>

    <div v-if="!railShell" class="sidebar-body">
      <section class="side-section">
        <div class="side-section-head">
          <span class="side-section-title">{{ t('sidebar.workspaces') }}</span>
          <button
            class="icon-btn"
            :title="t('sidebar.newWorkspace')"
            :aria-label="t('sidebar.newWorkspace')"
            @click="openWorkspaceDialog"
          >
            <Plus :size="16" />
          </button>
        </div>
        <SessionList />
      </section>
    </div>

    <div v-if="!railShell" class="footer-bar">
      <button
        v-if="statusStore.hasStatusSurface"
        class="footer-settings-btn"
        @click="statusOpen = true"
      >
        <span class="status-lamp" :class="'lamp-' + statusStore.lamp" />
        <Activity :size="15" />
        <span>{{ t("status.entry") }}</span>
      </button>
      <button class="footer-settings-btn" @click="settings.openPanel()">
        <Settings :size="16" />
        <span>{{ t("sidebar.settings") }}</span>
      </button>
    </div>

    <div v-if="wsDialogOpen" class="overlay" @click.self="wsDialogOpen = false">
      <div class="card ws-dialog">
        <div class="card-head">
          <span class="card-title">{{ t("sidebar.newWorkspace") }}</span>
          <button
            class="icon-btn"
            :title="t('common.close')"
            @click="wsDialogOpen = false"
          >
            <X :size="15" />
          </button>
        </div>
        <div class="card-body">
          <label class="field-label" for="ws-path-input">{{
            t("sidebar.workspacePathLabel")
          }}</label>
          <input
            id="ws-path-input"
            ref="wsInput"
            v-model="wsPath"
            class="field"
            type="text"
            :placeholder="t('sidebar.pathPlaceholder')"
            @keydown.enter="createWorkspace"
            @keydown.esc="wsDialogOpen = false"
          />
          <p class="ws-dialog-hint">{{ t("sidebar.newWorkspaceHint") }}</p>
          <button class="btn ws-dialog-browse" @click="pickerOpen = true">
            <FolderOpen :size="15" />
            {{ t("fs.browse") }}
          </button>
        </div>
        <div class="interrupt-actions">
          <button class="btn" @click="wsDialogOpen = false">
            {{ t("common.cancel") }}
          </button>
          <button
            class="btn btn-primary"
            :disabled="!wsPath.trim()"
            @click="createWorkspace"
          >
            {{ t("sidebar.create") }}
          </button>
        </div>
      </div>
    </div>

    <FolderPicker
      v-if="pickerOpen"
      @close="pickerOpen = false"
      @select="pickWorkspace"
    />
    <StatusModal v-if="statusOpen" @close="statusOpen = false" />
  </aside>
</template>
