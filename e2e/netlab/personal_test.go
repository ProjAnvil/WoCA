//go:build netlab

package netlab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPersonalRelayChain — personal-edition network topology: the public
// web frontend (client, front network only) → the relay → the outbound
// long-poll tunnel → home-network woca (wan network only). The chain must
// work end to end in the forward direction (login, tunnel business API and
// SSE all cross the network boundary), while the home-side addresses must
// be unreachable from the public side (docker network isolation = NAT).
func TestPersonalRelayChain(t *testing.T) {
	requireDocker(t)
	requireTarballs(t, "woca", "woca-relay")
	st := stack{name: "netlab personal", file: personalCompose}
	st.lifecycle(t)

	// Phase 1 — the relay first: the wcr_ key is minted ON the relay
	// (printed once) and only then handed to the home side, exactly the
	// real ops flow.
	st.up(t, nil, "relay", "client")
	// The networks exist now — arm the zone matrix (client's front and
	// the home uplink wan must be pairwise unreachable; see harness).
	isolate(t, [2]string{"woca-netlab-p-front", "woca-netlab-p-wan"})
	waitFor(t, 90*time.Second, "relay never became healthy", func() (bool, string) {
		return st.probeInContainer("relay", "http://127.0.0.1:8080/api/health")
	})

	out, err := st.execOut(t, "relay", "./woca-relay", "--db", "/var/lib/woca-relay/relay.db", "key", "create", "netlab")
	if err != nil {
		t.Fatalf("key create: %v", err)
	}
	key := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "wcr_") {
			key = strings.TrimSpace(line)
		}
	}
	if key == "" {
		t.Fatalf("key create printed no wcr_ key: %q", out)
	}
	t.Logf("relay key minted: %s…", key[:12])

	// Phase 2 — the home instance with the key: the tunnel comes up as
	// part of serve via the WOCA_RELAY_URL/KEY first-boot env path.
	st.up(t, map[string]string{"NETLAB_RELAY_KEY": key}, "woca")
	waitFor(t, 90*time.Second, "home woca never became healthy", func() (bool, string) {
		return st.probeInContainer("woca", "http://127.0.0.1:8080/api/health")
	})

	// P1 — the frontend reaches the relay's public surface.
	st.reachableFromClient(t, "http://relay:8080/api/health", 200)

	// P2 — login across the boundary, then a tunneled business API call
	// that really executes on the home instance (first calls wait out a
	// poll cycle, hence the retry).
	waitFor(t, 120*time.Second, "login through the relay never succeeded", func() (bool, string) {
		code, body := st.login(t, "http://relay:8080", key, "/tmp/netlab.jar")
		return code == 200 && strings.Contains(body, `"token"`), fmt.Sprintf("login = %d, body = %s", code, body)
	})
	waitFor(t, 120*time.Second, "tunneled /api/sessions never answered", func() (bool, string) {
		code, body := st.tunneledGet(t, "http://relay:8080", "/api/sessions", "/tmp/netlab.jar")
		return code == 200 && strings.HasPrefix(strings.TrimSpace(body), "["), fmt.Sprintf("GET = %d, body = %s", code, body)
	})

	// P3 — SSE survives the tunnel: content type and the no-buffering
	// header contract (mirrors ci/e2e-relay.sh).
	headers := st.sseHeaders(t, "http://relay:8080", "/tmp/netlab.jar")
	for _, want := range []string{"text/event-stream", "x-accel-buffering: no"} {
		if !strings.Contains(strings.ToLower(headers), want) {
			t.Fatalf("SSE headers through the relay lack %q:\n%s", want, headers)
		}
	}

	// E1 — a wrong key errors AT the relay boundary: 401 immediately,
	// never a hang.
	code, body := st.login(t, "http://relay:8080", "wcr_netlab-bogus-key", "/tmp/netlab.bad.jar")
	if code != 401 {
		t.Fatalf("bogus-key login = %d (want 401), body: %s", code, body)
	}

	// N1 — isolation: the home instance's wan address must NOT be
	// reachable from the public side, by IP (L3) and by name (no shared
	// docker DNS) — only the tunnel crosses.
	homeIP := containerIP(t, "woca-netlab-p-woca", "woca-netlab-p-wan")
	st.unreachableFromClient(t, "http://"+homeIP+":8080/api/health")
	st.unreachableFromClient(t, "http://woca:8080/api/health")

	t.Logf("personal relay chain green: client(front) → relay → tunnel → woca(wan %s)", homeIP)
}

// probeInContainer runs a busybox-wget health probe inside one service.
func (s stack) probeInContainer(service, url string) (bool, string) {
	out, err := s.execE(service, "wget", "-q", "-T", "5", "-O", "/dev/null", url)
	if err != nil {
		return false, fmt.Sprintf("wget %s: %v %s", url, err, out)
	}
	return true, ""
}
