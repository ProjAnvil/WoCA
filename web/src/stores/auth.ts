import { ref } from "vue";
import { defineStore } from "pinia";
import { getMe, listSessions, UnauthorizedError, type Me } from "../api/client";

// The bridge login gate: any 401 from the API raises the overlay; a
// successful login reloads the data stores. On the loopback personal
// deployment no request ever 401s and the gate never appears. `me` carries
// the login identity (remark + role) for the admin surface — null on
// un-managed deployments without /api/auth/me.
export const useAuthStore = defineStore("auth", () => {
  const gateOpen = ref(false);
  const gateCode = ref<string>("");
  const me = ref<Me | null>(null);

  async function probe() {
    try {
      await listSessions();
    } catch (e) {
      if (e instanceof UnauthorizedError) {
        gateOpen.value = true;
        gateCode.value = e.code ?? "unauthorized";
      }
      return;
    }
    try {
      me.value = await getMe();
    } catch {
      // Un-managed deployments have no /api/auth/me — not an error.
    }
  }

  function raise(code = "unauthorized") {
    gateOpen.value = true;
    gateCode.value = code;
  }

  async function onAuthenticated() {
    gateOpen.value = false;
    gateCode.value = "";
    // The reload refreshes the data stores, but it would also kill the gate's
    // 320ms dissolve + app-rise handoff (motion.css) mid-flight — so it waits
    // one animation window. Timing only: the reload itself is unconditional.
    window.setTimeout(() => window.location.reload(), 380);
  }

  return { gateOpen, gateCode, me, probe, raise, onAuthenticated };
});
